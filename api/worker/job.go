package worker

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/helpers"
	"github.com/alfg/openencoder/api/types"
)

// jobRun holds the state of a single execution of a job.
//
// Every execution owns a unique run id. All writes made on its behalf are
// conditional on that run id, so a cancelled or restarted execution can never
// overwrite the progress or the status of the execution that replaced it.
type jobRun struct {
	l     *Lifecycle
	job   types.Job
	runID string

	ctx    context.Context
	cancel context.CancelFunc
}

// Run executes a job from download to cleanup and returns the status that was
// persisted.
//
// The lifecycle converges on exactly one terminal status:
//   - a job that completes normally ends as 'completed' after upload+cleanup,
//   - an operator cancel ends as 'cancelled' and stops the running stage,
//   - a failure ends as 'error',
//   - a job that was restarted while this execution was running is left to the
//     new execution (this execution stops writing and removes its own files).
//
// The returned error only reports infrastructure failures that prevented the job
// from being finalized; a cancellation or a superseded execution is not an
// error.
func (l *Lifecycle) Run(ctx context.Context, job types.Job) (string, error) {
	l.applyDefaults()

	runID := l.newRunID()

	state, claimed, err := l.Jobs.ClaimJobRun(job.GUID, runID)
	if err != nil {
		return "", err
	}
	if !claimed {
		// Another execution owns the job, or it was cancelled before it
		// started. Do nothing so we never write over a live execution.
		if state == nil {
			return "", nil
		}
		return state.Status, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)

	r := &jobRun{
		l:      l,
		job:    job,
		runID:  runID,
		ctx:    runCtx,
		cancel: cancel,
	}

	// Each execution gets its own working directory, and leftovers of older
	// executions of the same job are removed so a restart starts clean.
	r.job.LocalSource = helpers.CreateRunSourcePath(
		l.WorkDir, job.Source, job.GUID, runID)
	if err := helpers.RemoveStaleRunTmpPaths(l.WorkDir, job.GUID, runID); err != nil {
		log.Warn("failed to remove stale job directories: ", err)
	}

	monitorDone := make(chan struct{})
	go r.monitor(monitorDone)

	final, runErr := r.execute()

	// Stop the cancellation monitor and wait for it before returning, so no
	// watcher survives a finished execution.
	cancel()
	<-monitorDone

	// Release the run binding (a no-op if a newer execution already owns the
	// job) so a cancelled or failed job can be claimed again later.
	if err := l.Jobs.ReleaseJobRun(job.GUID, runID); err != nil {
		log.Warn("failed to release job run: ", err)
	}

	// Whatever happened - completed, cancelled, failed or superseded - this
	// execution must not leave temporary files behind.
	if err := helpers.RemoveRunTmpPath(l.WorkDir, job.GUID, runID); err != nil {
		log.Warn("failed to remove job working directory: ", err)
	}
	helpers.RemoveEmptyJobDir(l.WorkDir, job.GUID)

	return final, runErr
}

// execute runs every stage in order and maps the outcome to a terminal status.
//
// The decision is made from the persisted state of the job, not from the local
// context alone, because cancellation, restart and completion can interleave
// arbitrarily:
//
//   - the job row says 'cancelled' (the operator cancelled): it stays cancelled
//     and this execution writes nothing else,
//   - the job is owned by a different run id (restart) or is already final:
//     leave the row to its owner,
//   - otherwise this execution owns a live job and records the outcome.
func (r *jobRun) execute() (string, error) {
	stageErr := r.pipeline()
	if stageErr == nil {
		return r.finalize(types.JobCompleted)
	}
	log.Error("job stage failed: ", stageErr)

	status, stateErr := r.l.Jobs.GetJobRunStateByGUID(r.job.GUID)
	if stateErr != nil {
		return "", stateErr
	}

	// Cancellation wins over every other outcome. The cancel flag is enough:
	// the operator asked for the job to stop, so it must not be reported as
	// completed or failed.
	if status.CancelRequested || status.Status == types.JobCancelled {
		if status.Status == types.JobCancelled {
			return status.Status, nil
		}
		return r.finalize(types.JobCancelled)
	}

	// Another execution took over (restart) or the job is already terminal.
	// Leave the row alone: the new owner is responsible for it.
	if status.RunID.String != r.runID {
		return status.Status, nil
	}

	return r.finalize(types.JobError)
}

// pipeline runs the job stages. It returns nil on success, or the first error
// that stopped the execution.
func (r *jobRun) pipeline() error {
	l := r.l
	job := &r.job

	storageDriver, err := l.Settings.GetSetting(types.StorageDriver)
	if err != nil {
		return err
	}

	streaming, err := l.Settings.GetSetting(types.S3Streaming)
	if err != nil {
		return err
	}

	if err := r.setStatus(types.JobDownloading); err != nil {
		return err
	}

	if streaming.Value == "enabled" && storageDriver.Value == "s3" {
		// Get a presigned URL and encode directly from storage.
		presigned, err := l.Transfer.GetPresignedURL(*job)
		if err != nil {
			return err
		}
		job.Source = presigned
	} else {
		if err := l.Transfer.Download(r.ctx, *job, r.transferReporter()); err != nil {
			return err
		}
		if err := r.setTransferComplete(); err != nil {
			return err
		}
		job.Source = job.LocalSource
	}

	if err := r.setStatus(types.JobProbing); err != nil {
		return err
	}
	probeData, err := l.Prober.Run(r.ctx, job.Source)
	if err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if err := r.storeProbe(probeData); err != nil {
		return err
	}

	if err := r.encodeStage(probeData); err != nil {
		return err
	}

	if err := r.setStatus(types.JobUploading); err != nil {
		return err
	}
	if err := l.Transfer.Upload(r.ctx, *job, r.transferReporter()); err != nil {
		return err
	}
	if err := r.setTransferComplete(); err != nil {
		return err
	}

	// Cleanup is an explicit stage on the happy path so that upload always
	// happens before the working files are removed.
	if err := r.cleanupStage(); err != nil {
		return err
	}
	return nil
}

func (r *jobRun) encodeStage(probeData *encoder.FFProbeResponse) error {
	if err := r.setStatus(types.JobEncoding); err != nil {
		return err
	}

	preset, err := r.l.Presets.GetPresetByName(r.job.Preset)
	if err != nil {
		return err
	}
	dest := r.destinationPath(preset.Output)

	if err := r.l.Jobs.UpdateEncodeOptionsForRun(r.job.GUID, r.runID, preset.Data); err != nil {
		return err
	}

	enc := r.l.NewEncoder()
	defer enc.Done()

	stop := make(chan struct{})
	progressDone := make(chan struct{})
	go r.reportEncodeProgress(enc, probeData, stop, progressDone)

	err = enc.Run(r.ctx, r.job.Source, dest, preset.Data)

	// Make sure the progress reporter has exited before returning, so no
	// progress write can happen after this stage has finished.
	close(stop)
	<-progressDone

	if err != nil && (r.ctx.Err() != nil || errors.Is(err, encoder.ErrCancelled)) {
		return types.ErrCancelled
	}
	return err
}

// reportEncodeProgress periodically persists encode progress for this run. It
// exits as soon as the encode stops, the job is cancelled, or a write is
// rejected because this execution no longer owns the job.
func (r *jobRun) reportEncodeProgress(enc Encoder, probeData *encoder.FFProbeResponse, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	totalFrames := probeTotalFrames(probeData)
	if totalFrames == 0 {
		return
	}

	ticker := time.NewTicker(r.l.ProgressInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			snapshot := enc.ProgressSnapshot()
			pct := math.Round((float64(snapshot.Frame)/float64(totalFrames))*100*100) / 100
			log.Infof("progress: %d / %d - %0.2f%%", snapshot.Frame, totalFrames, pct)
			if err := r.l.Jobs.UpdateEncodeProgressForRun(
				r.job.GUID, r.runID, pct, snapshot.Speed, snapshot.FPS); err != nil {
				// The run lost ownership (cancel/restart/finished): stop the
				// stage right away instead of waiting for the next poll.
				r.cancel()
				return
			}
		}
	}
}

// transferReporter returns a progress callback bound to this run. Progress is
// dropped silently if the execution no longer owns the job.
func (r *jobRun) transferReporter() func(float64) {
	return func(p float64) {
		if err := r.l.Jobs.UpdateTransferProgressForRun(r.job.GUID, r.runID, p); err != nil {
			if errors.Is(err, types.ErrStaleRun) {
				// This execution no longer owns the job: abort the transfer.
				r.cancel()
				return
			}
			log.Warn("failed to update transfer progress: ", err)
		}
	}
}

func (r *jobRun) destinationPath(output string) string {
	return helpers.GetRunTmpPath(r.l.WorkDir, r.job.GUID, r.runID) + "/dst/" + output
}

func (r *jobRun) storeProbe(probeData *encoder.FFProbeResponse) error {
	b, err := json.Marshal(probeData)
	if err != nil {
		return err
	}
	return r.l.Jobs.UpdateEncodeProbeForRun(r.job.GUID, r.runID, string(b))
}

func (r *jobRun) cleanupStage() error {
	log.Info("running cleanup task")
	return helpers.RemoveRunTmpPath(r.l.WorkDir, r.job.GUID, r.runID)
}

// setStatus persists an intermediate status for this run. It fails with
// types.ErrCancelled when the context is already cancelled and with
// types.ErrStaleRun when the job was cancelled, restarted or finished meanwhile.
func (r *jobRun) setStatus(status string) error {
	if err := r.ctx.Err(); err != nil {
		return types.ErrCancelled
	}
	return r.l.Jobs.UpdateJobRunStatus(r.job.GUID, r.runID, status)
}

func (r *jobRun) setTransferComplete() error {
	if err := r.l.Jobs.UpdateTransferProgressForRun(r.job.GUID, r.runID, 100); err != nil {
		if errors.Is(err, types.ErrStaleRun) {
			return types.ErrCancelled
		}
		return err
	}
	return nil
}

// finalize writes the terminal status of this run. It returns the status that
// was actually persisted, which may differ from the requested one when an
// operator cancel races with completion (cancellation wins).
func (r *jobRun) finalize(status string) (string, error) {
	final, err := r.l.Jobs.FinishJobRun(r.job.GUID, r.runID, status)
	if err != nil {
		if errors.Is(err, types.ErrStaleRun) {
			// The job is already terminal or another execution owns it.
			return final, nil
		}
		return "", err
	}
	if final == types.JobCompleted {
		r.sendAlert()
	}
	return final, nil
}

func (r *jobRun) sendAlert() {
	if r.l.Alerts == nil {
		return
	}
	if err := r.l.Alerts.Send(r.job); err != nil {
		log.Error(err)
	}
}

// monitor watches the job row while this execution runs. When the operator
// cancels the job or another execution takes it over, it cancels the context,
// which immediately stops the running stage: exec.CommandContext kills an
// FFmpeg process, transfers abort and sleepers wake up.
func (r *jobRun) monitor(done chan<- struct{}) {
	defer close(done)

	ticker := time.NewTicker(r.l.CancelPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			state, err := r.l.Jobs.GetJobRunStateByGUID(r.job.GUID)
			if err != nil {
				log.Warn("failed to poll job state: ", err)
				continue
			}
			cancelled := state.Status == types.JobCancelled || state.CancelRequested
			superseded := state.RunID.String != r.runID
			// A terminal status that this execution did not write itself means
			// the job was finalized elsewhere (for example by an operator): stop
			// the stage instead of continuing to run work nobody will read.
			finalizedElsewhere := types.IsTerminalStatus(state.Status)
			if cancelled || superseded || finalizedElsewhere {
				log.Infof("stopping job %s (cancelled=%t superseded=%t finalized=%t)",
					r.job.GUID, cancelled, superseded, finalizedElsewhere)
				r.cancel()
				return
			}
		}
	}
}

// probeTotalFrames extracts the frame count from probe data.
func probeTotalFrames(p *encoder.FFProbeResponse) int {
	if p == nil || len(p.Streams) == 0 {
		return 0
	}
	frames, err := strconv.Atoi(p.Streams[0].NbFrames)
	if err != nil {
		return 0
	}
	return frames
}
