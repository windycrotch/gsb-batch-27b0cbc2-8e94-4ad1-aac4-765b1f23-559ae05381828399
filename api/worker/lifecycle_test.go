package worker

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/helpers"
	"github.com/alfg/openencoder/api/types"
)

// lifecycleHarness bundles a Lifecycle wired to fakes.
type lifecycleHarness struct {
	t          *testing.T
	store      *fakeStore
	transfer   *fakeTransfer
	encoder    *fakeEncoder
	prober     *fakeProber
	workDir    string
	markerPath string
}

func newHarness(t *testing.T) *lifecycleHarness {
	t.Helper()

	return &lifecycleHarness{
		t:        t,
		store:    newFakeStore(types.JobQueued),
		transfer: newFakeTransfer(),
		encoder:  newFakeEncoder(),
		prober:   &fakeProber{},
		workDir:  tempDir(t),
	}
}

func (h *lifecycleHarness) lifecycle(runIDs ...string) *Lifecycle {
	var mu sync.Mutex
	i := 0
	l := &Lifecycle{
		Jobs: h.store,
		Settings: fakeSettings{values: map[string]string{
			types.StorageDriver: "s3",
			types.S3Streaming:   "disabled",
		}},
		Presets:            fakePresets{},
		Transfer:           h.transfer,
		Prober:             h.prober,
		Alerts:             nil,
		WorkDir:            h.workDir,
		NewEncoder:         func() Encoder { return h.encoder },
		ProgressInterval:   2 * time.Millisecond,
		CancelPollInterval: 2 * time.Millisecond,
	}
	if len(runIDs) > 0 {
		l.NewRunID = func() string {
			mu.Lock()
			defer mu.Unlock()
			if i >= len(runIDs) {
				return "extra-run-" + string(rune('a'+i))
			}
			id := runIDs[i]
			i++
			return id
		}
	}
	return l
}

func (h *lifecycleHarness) job() types.Job {
	return types.Job{
		GUID:        "job-guid",
		Preset:      "preset",
		Source:      "source.mp4",
		Destination: "out/",
	}
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestNormalCompletionRunsStagesInOrder verifies that the happy path walks the
// intermediate statuses in order, uploads before cleaning up, and ends
// completed with no temporary files left behind.
func TestNormalCompletionRunsStagesInOrder(t *testing.T) {
	h := newHarness(t)
	h.transfer.reportProgress = true

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("final status = %q, want %q", final, types.JobCompleted)
	}

	got := strings.Join(h.store.statuses(), ",")
	want := strings.Join([]string{
		types.JobDownloading, types.JobProbing, types.JobEncoding,
		types.JobUploading, types.JobCompleted,
	}, ",")
	if got != want {
		t.Fatalf("status order = %s, want %s", got, want)
	}

	downloads, uploads := h.transfer.counts()
	if downloads != 1 || uploads != 1 {
		t.Fatalf("downloads=%d uploads=%d, want 1/1", downloads, uploads)
	}

	// Upload must happen before the working directory is removed: on completion
	// cleanup runs as an explicit stage, so encoded files are still present when
	// the uploader is invoked.
	if h.transfer.uploadSawMissingFiles {
		t.Fatal("upload ran after the working directory was removed")
	}

	// No temporary files left after the job terminates.
	assertNoLeftovers(t, h.workDir, h.job().GUID)

	// The encoder is always told it is done and never cancelled on success.
	if !h.encoder.wasDone() {
		t.Fatal("encoder.Done() was not called on completion")
	}
}

// TestCancelStopsRunningStagePromptly verifies that cancelling while the encode
// is running aborts the encode quickly (well before the next progress poll would
// have noticed it), keeps the final status cancelled, and removes temp files.
func TestCancelStopsRunningStagePromptly(t *testing.T) {
	h := newHarness(t)
	h.encoder.block = true

	// A long progress interval proves that cancellation does not depend on the
	// next progress tick. The state watcher is the only thing that can stop the
	// blocked encode here.
	l := h.lifecycle("run-1")
	l.ProgressInterval = time.Hour

	result := make(chan string, 1)
	go func() {
		final, err := l.Run(context.Background(), h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		result <- final
	}()

	waitFor(t, time.Second, func() bool { return h.encoder.wasStarted() }, "encode to start")

	// Operator cancels through the same code path the API uses.
	if ok, err := h.store.RequestJobCancelByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("cancel request failed: ok=%v err=%v", ok, err)
	}

	// The blocked fake encoder only returns when its context is cancelled, just
	// like a real ffmpeg process being killed.
	select {
	case final := <-result:
		if final != types.JobCancelled {
			t.Fatalf("final status = %q, want cancelled", final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled job did not stop promptly")
	}

	assertNoLeftovers(t, h.workDir, h.job().GUID)

	_, uploads := h.transfer.counts()
	if uploads != 0 {
		t.Fatalf("upload ran for a cancelled job: %d uploads", uploads)
	}
}

// TestCancelDuringUploadAbortsUpload covers a cancel that arrives after the
// encode finished: the upload stage must be aborted and the job must end
// cancelled, not completed.
func TestCancelDuringUploadAbortsUpload(t *testing.T) {
	h := newHarness(t)
	h.transfer.blockUpload = true

	result := make(chan string, 1)
	go func() {
		final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		result <- final
	}()

	waitFor(t, time.Second, func() bool { return h.transfer.uploadsStarted() }, "upload to start")

	if ok, err := h.store.RequestJobCancelByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("cancel request failed: ok=%v err=%v", ok, err)
	}

	select {
	case final := <-result:
		if final != types.JobCancelled {
			t.Fatalf("final status = %q, want cancelled", final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not abort after cancel")
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestCancelledJobIsNotClaimed verifies that a worker that picks up an
// already-cancelled job does nothing at all.
func TestCancelledJobIsNotClaimed(t *testing.T) {
	h := newHarness(t)
	h.store.status = types.JobCancelled

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCancelled {
		t.Fatalf("final status = %q, want cancelled", final)
	}
	if got := len(h.store.claims()); got != 0 {
		t.Fatalf("cancelled job was claimed %d times", got)
	}
	if downloads, uploads := h.transfer.counts(); downloads != 0 || uploads != 0 {
		t.Fatalf("cancelled job transferred files: downloads=%d uploads=%d", downloads, uploads)
	}
	if h.prober.callCount() != 0 {
		t.Fatal("cancelled job was probed")
	}
}

// TestLateProgressAfterRestartIsDiscarded is the core regression test for the
// "two progress writers overwrite each other" bug: a restarted job must ignore
// writes coming from the previous execution.
func TestLateProgressAfterRestartIsDiscarded(t *testing.T) {
	h := newHarness(t)
	h.encoder.block = true

	oldRun := make(chan string, 1)
	go func() {
		final, err := h.lifecycle("old-run").Run(context.Background(), h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		oldRun <- final
	}()

	waitFor(t, time.Second, func() bool { return h.encoder.wasStarted() }, "old encode to start")

	// The old execution owns the job now.
	if status, runID, _ := h.store.snapshot(); status != types.JobEncoding || runID != "old-run" {
		t.Fatalf("unexpected state before restart: status=%q runID=%q", status, runID)
	}

	// Operator restarts the job. This clears the run binding and resets progress.
	if ok, err := h.store.RestartJobRunByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("restart failed: ok=%v err=%v", ok, err)
	}

	// The old execution's progress writer must now be rejected. Simulate a late
	// write the way a tracker would: directly through the store with the old run
	// id.
	if err := h.store.UpdateEncodeProgressForRun(h.job().GUID, "old-run", 99, "9x", 999); err == nil {
		t.Fatal("late progress write from the old run was accepted")
	}

	h.encoder.forceRelease()
	select {
	case <-oldRun:
	case <-time.After(2 * time.Second):
		t.Fatal("old execution did not stop after restart")
	}

	status, runID, _ := h.store.snapshot()
	if status != types.JobRestarting {
		t.Fatalf("status after restart = %q, want restarting", status)
	}
	if runID != "" {
		t.Fatalf("run id after restart = %q, want empty", runID)
	}

	// Every write the old run attempted after the restart must be rejected.
	for _, w := range h.store.acceptedWritesFor("old-run") {
		if w.accepted && w.progress == 99 {
			t.Fatal("stale progress write was accepted after restart")
		}
	}

	// Progress left behind must not leak into the new execution: the claim
	// resets progress.
	h.store.mu.Lock()
	h.store.progress = 88
	h.store.mu.Unlock()

	h2 := newHarness(t)
	h2.store = h.store
	h2.workDir = h.workDir
	h2.encoder = newFakeEncoder()

	final, err := h2.lifecycle("new-run").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("new execution failed: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("new execution final status = %q, want completed", final)
	}
	if _, _, progress := h.store.snapshot(); progress != 100 {
		t.Fatalf("progress after restart = %v, want the new run's 100", progress)
	}
	assertNoLeftovers(t, h2.workDir, h.job().GUID)
}

// TestRestartSupersedesOldRunByProgressRejection proves that the old execution
// is stopped by its own rejected writes (restart can happen while the progress
// tracker is writing) and that the old run never observes a success.
func TestRestartSupersedesOldRunByProgressRejection(t *testing.T) {
	h := newHarness(t)
	h.encoder.block = true
	l := h.lifecycle("old-run")
	// Poll slowly so that only the rejected progress write can stop the run.
	l.CancelPollInterval = time.Hour
	l.ProgressInterval = 2 * time.Millisecond

	result := make(chan string, 1)
	go func() {
		final, err := l.Run(context.Background(), h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		result <- final
	}()

	waitFor(t, time.Second, func() bool { return h.encoder.wasStarted() }, "encode to start")
	// Let the progress tracker write at least once.
	waitFor(t, time.Second, func() bool {
		return len(h.store.acceptedWritesFor("old-run")) > 0
	}, "progress write")

	if ok, err := h.store.RestartJobRunByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("restart failed: ok=%v err=%v", ok, err)
	}

	// Only the rejected progress write can stop this run (the state watcher
	// polls once an hour); the fake encoder returns as soon as its context is
	// cancelled, like a real killed process.
	select {
	case final := <-result:
		if final == types.JobCompleted {
			t.Fatal("superseded execution reported completion")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("superseded execution kept running")
	}

	status, runID, _ := h.store.snapshot()
	if status != types.JobRestarting || runID != "" {
		t.Fatalf("job state after restart = %q/%q, want restarting/empty", status, runID)
	}
}

// TestCompletedStatusIsNotRevertedByLateProgress verifies the completion and
// cancel race: a late progress write can not move a completed job back to an
// intermediate status, and a cancel that lands after completion does not undo
// completion.
func TestCompletedStatusIsNotRevertedByLateProgress(t *testing.T) {
	h := newHarness(t)

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("final status = %q, want completed", final)
	}

	// A late progress write with the (now released) run id is rejected.
	if err := h.store.UpdateEncodeProgressForRun(h.job().GUID, "run-1", 42, "1x", 24); err == nil {
		t.Fatal("late progress write after completion was accepted")
	}
	// A late status write is rejected too.
	if err := h.store.UpdateJobRunStatus(h.job().GUID, "run-1", types.JobEncoding); err == nil {
		t.Fatal("late status write after completion was accepted")
	}
	// Cancelling a completed job is a no-op.
	if ok, _ := h.store.RequestJobCancelByGUID(h.job().GUID); ok {
		t.Fatal("completed job was cancelled")
	}

	status, _, _ := h.store.snapshot()
	if status != types.JobCompleted {
		t.Fatalf("status = %q, want completed", status)
	}
}

// TestCancelWinsWhenItRacesCompletion verifies that a cancel request that
// arrives while the final stage is finishing produces exactly one status,
// 'cancelled', regardless of which code path observes it last.
func TestCancelWinsWhenItRacesCompletion(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newHarness(t)

		// Cancel right before the finalize write: emulate the operator hitting
		// cancel while the upload/cleanup stage is finishing.
		h.store.beforeFinish = func() {
			h.store.RequestJobCancelByGUID(h.job().GUID)
		}

		final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if final != types.JobCancelled {
			t.Fatalf("iteration %d: final status = %q, want cancelled", i, final)
		}

		// Exactly one terminal status must be recorded.
		terminals := 0
		for _, s := range h.store.statuses() {
			if types.IsTerminalStatus(s) {
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("iteration %d: %d terminal statuses recorded: %v", i, terminals, h.store.statuses())
		}
	}
}

// TestFailureEndsInError covers a non-cancellation failure.
func TestFailureEndsInError(t *testing.T) {
	h := newHarness(t)
	h.transfer.uploadErr = context.DeadlineExceeded

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobError {
		t.Fatalf("final status = %q, want error", final)
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestRunWorkingDirectoriesAreIsolated verifies that every execution uses its
// own directory and that a new execution removes leftovers of previous ones.
func TestRunWorkingDirectoriesAreIsolated(t *testing.T) {
	workDir := tempDir(t)
	guid := "job-guid"

	oldSource := helpers.CreateRunSourcePath(workDir, "source.mp4", guid, "old-run")
	if err := ioutil.WriteFile(oldSource, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}

	helpers.CreateRunSourcePath(workDir, "source.mp4", guid, "new-run")
	if err := helpers.RemoveStaleRunTmpPaths(workDir, guid, "new-run"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(workDir, guid, "old-run")); !os.IsNotExist(err) {
		t.Fatal("stale run directory was not removed")
	}
	if _, err := os.Stat(filepath.Join(workDir, guid, "new-run", "dst")); err != nil {
		t.Fatalf("new run directory missing: %v", err)
	}
}

// TestStreamingSourceSkipsDownload verifies the S3 streaming path: a presigned
// URL is used as the source and no download happens.
func TestStreamingSourceSkipsDownload(t *testing.T) {
	h := newHarness(t)
	l := h.lifecycle("run-1")
	l.Settings = fakeSettings{values: map[string]string{
		types.StorageDriver: "s3",
		types.S3Streaming:   "enabled",
	}}

	final, err := l.Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("final status = %q, want completed", final)
	}
	if downloads, uploads := h.transfer.counts(); downloads != 0 || uploads != 1 {
		t.Fatalf("streaming job: downloads=%d uploads=%d, want 0/1", downloads, uploads)
	}
	// The probe and the encoder must work directly from the presigned URL.
	if got := h.prober.lastInput(); got != "https://example.invalid/presigned" {
		t.Fatalf("probe input = %q, want the presigned URL", got)
	}
}

// TestProbeWithoutFrameCountStillCompletes verifies a job whose probe data has
// no frame count: the progress tracker can not compute a percentage but the job
// still completes normally.
func TestProbeWithoutFrameCountStillCompletes(t *testing.T) {
	h := newHarness(t)
	h.prober = &fakeProber{noFrames: true}

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("final status = %q, want completed", final)
	}
	if writes := h.store.acceptedWritesFor("run-1"); len(writes) != 0 {
		t.Fatalf("progress written without a frame count: %v", writes)
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestProbeFailureEndsInError covers a failing probe stage: no encode/upload
// happens and the job ends as error with no leftovers.
func TestProbeFailureEndsInError(t *testing.T) {
	h := newHarness(t)
	h.prober = &fakeProber{err: context.DeadlineExceeded}

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobError {
		t.Fatalf("final status = %q, want error", final)
	}
	if _, uploads := h.transfer.counts(); uploads != 0 {
		t.Fatal("upload ran after a failed probe")
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestEncodeFailureEndsInError covers a failing encode stage.
func TestEncodeFailureEndsInError(t *testing.T) {
	h := newHarness(t)
	h.encoder.runErr = errEncodeFailed

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobError {
		t.Fatalf("final status = %q, want error", final)
	}
	if _, uploads := h.transfer.counts(); uploads != 0 {
		t.Fatal("upload ran after a failed encode")
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestAbandonedFilesFromCrashedRunsAreRemoved verifies that leftovers of a
// previous crashed execution are removed when a new execution starts.
func TestAbandonedFilesFromCrashedRunsAreRemoved(t *testing.T) {
	h := newHarness(t)

	// Simulate a crash: a previous run left its directory behind without ever
	// cleaning up.
	stale := helpers.CreateRunSourcePath(h.workDir, "source.mp4", h.job().GUID, "crashed-run")
	if err := ioutil.WriteFile(stale, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}

	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("final status = %q, want completed", final)
	}
	if _, err := os.Stat(filepath.Join(h.workDir, h.job().GUID, "crashed-run")); !os.IsNotExist(err) {
		t.Fatal("files from a crashed run were not removed")
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestAlertSentOnlyOnCompletion verifies the alert hook is only used for
// successful completions.
func TestAlertSentOnlyOnCompletion(t *testing.T) {
	h := newHarness(t)
	alert := &fakeAlerter{}

	l := h.lifecycle("run-1")
	l.Alerts = alert
	if _, err := l.Run(context.Background(), h.job()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert.count() != 1 {
		t.Fatalf("alerts sent on completion = %d, want 1", alert.count())
	}

	h2 := newHarness(t)
	alert2 := &fakeAlerter{}
	l2 := h2.lifecycle("run-2")
	l2.Alerts = alert2
	h2.transfer.uploadErr = context.DeadlineExceeded
	if _, err := l2.Run(context.Background(), h2.job()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert2.count() != 0 {
		t.Fatalf("alerts sent on failure = %d, want 0", alert2.count())
	}
}

// TestCancelDuringDownloadAbortsDownload covers a cancel that arrives before the
// encode starts: the transfer must be aborted, no encode/upload must run, and
// the job must end cancelled.
func TestCancelDuringDownloadAbortsDownload(t *testing.T) {
	h := newHarness(t)
	h.transfer.blockDownload = true

	result := make(chan string, 1)
	go func() {
		final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		result <- final
	}()

	waitFor(t, time.Second, func() bool {
		downloads, _ := h.transfer.counts()
		return downloads > 0
	}, "download to start")

	if ok, err := h.store.RequestJobCancelByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("cancel request failed: ok=%v err=%v", ok, err)
	}

	select {
	case final := <-result:
		if final != types.JobCancelled {
			t.Fatalf("final status = %q, want cancelled", final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not abort after cancel")
	}

	if h.prober.callCount() != 0 {
		t.Fatal("probe ran after a cancelled download")
	}
	if h.encoder.wasStarted() {
		t.Fatal("encode ran after a cancelled download")
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

func assertNoLeftovers(t *testing.T, workDir, guid string) {
	t.Helper()
	path := filepath.Join(workDir, guid)
	if entries, err := ioutil.ReadDir(path); err == nil && len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left behind: %v", names)
	}
}

// TestRestartFromErrorStartsClean verifies that an operator can restart a job
// that previously failed: the new execution claims the job, starts from a clean
// working directory and completes, while the failed execution can no longer
// write to it.
func TestRestartFromErrorStartsClean(t *testing.T) {
	h := newHarness(t)

	// First execution fails at upload.
	h.transfer.uploadErr = context.DeadlineExceeded
	final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobError {
		t.Fatalf("first execution final status = %q, want error", final)
	}

	// The failed execution must not be able to write anymore.
	if err := h.store.UpdateEncodeProgressForRun(h.job().GUID, "run-1", 42, "1x", 24); err == nil {
		t.Fatal("failed execution wrote progress after it ended")
	}

	// Operator restarts and a new execution succeeds.
	h.transfer.uploadErr = nil
	if ok, err := h.store.RestartJobRunByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("restart failed: ok=%v err=%v", ok, err)
	}

	h2 := newHarness(t)
	h2.store = h.store
	h2.workDir = h.workDir
	h2.encoder = newFakeEncoder()

	final, err = h2.lifecycle("run-2").Run(context.Background(), h.job())
	if err != nil {
		t.Fatalf("restarted execution failed: %v", err)
	}
	if final != types.JobCompleted {
		t.Fatalf("restarted execution final status = %q, want completed", final)
	}
	assertNoLeftovers(t, h.workDir, h.job().GUID)
}

// TestConcurrentCancelAndCompletionHasSingleFinalStatus hammers the cancel /
// complete race from the operator side while the lifecycle finishes, and checks
// that the outcome is always exactly one terminal status.
func TestConcurrentCancelAndCompletionHasSingleFinalStatus(t *testing.T) {
	for i := 0; i < 25; i++ {
		h := newHarness(t)

		done := make(chan struct{})
		go func() {
			defer close(done)
			// Race the operator cancel against the lifecycle's own finalize.
			h.store.RequestJobCancelByGUID(h.job().GUID)
		}()

		final, err := h.lifecycle("run-1").Run(context.Background(), h.job())
		<-done
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if final != types.JobCancelled && final != types.JobCompleted {
			t.Fatalf("iteration %d: final status = %q, want cancelled or completed", i, final)
		}

		status, _, _ := h.store.snapshot()
		if status != final {
			t.Fatalf("iteration %d: snapshot status %q and returned status %q differ", i, status, final)
		}
		terminals := 0
		for _, s := range h.store.statuses() {
			if types.IsTerminalStatus(s) {
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("iteration %d: %d terminal statuses recorded: %v", i, terminals, h.store.statuses())
		}
	}
}

// errEncodeFailed is a generic stage failure used by tests.
var errEncodeFailed = errors.New("ffmpeg failed")

var _ = encoder.Snapshot{}

// tempDir creates a temporary directory removed at the end of the test. It
// predates t.TempDir so the tests build with the repository's Go version.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := ioutil.TempDir("", "openencoder-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
