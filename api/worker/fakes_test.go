package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/types"
)

// fakeStore is an in-memory implementation of the job persistence that mirrors
// the guard semantics of api/data/job_runs.go:
//
//   - an execution owns a job through its run id,
//   - every write is conditional on the run id and on the job not being
//     terminal,
//   - a cancel request moves the job to 'cancelled' immediately and sets
//     cancel_requested,
//   - finishing prefers 'cancelled' over the requested status,
//   - a new claim resets progress.
//
// It is intentionally conservative: anything the real SQL guards would reject is
// rejected here too.
type fakeStore struct {
	mu sync.Mutex

	status          string
	runID           string
	cancelRequested bool
	progress        float64
	speed           string
	fps             float64
	probe           string
	options         string

	// writeLog records every progress write attempt and whether it was
	// accepted, so tests can assert that stale executions never write.
	writeLog []progressWrite

	// statusLog records every accepted status transition in order.
	statusLog []string

	// claimOrder records the run ids that successfully claimed the job.
	claimOrder []string

	// onClaim is invoked (without the lock) after a successful claim.
	onClaim func(runID string)

	// beforeFinish is invoked (without the lock) right before a finish write,
	// so tests can interleave an operator cancel with completion.
	beforeFinish func()

	job types.Job
}

type progressWrite struct {
	runID    string
	progress float64
	accepted bool
}

func newFakeStore(status string) *fakeStore {
	return &fakeStore{
		status: status,
		job: types.Job{
			GUID:        "job-guid",
			Preset:      "preset",
			Source:      "source.mp4",
			Destination: "out/",
			Encode:      types.Encode{EncodeID: 1},
		},
	}
}

func (s *fakeStore) GetJobRunStateByGUID(guid string) (*types.JobRunState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &types.JobRunState{
		GUID:            s.job.GUID,
		Status:          s.status,
		RunID:           types.NullString{NullString: sqlNullString(s.runID)},
		CancelRequested: s.cancelRequested,
	}, nil
}

func (s *fakeStore) ClaimJobRun(guid string, runID string) (*types.JobRunState, bool, error) {
	s.mu.Lock()
	claimed := s.runID == "" && !s.cancelRequested && !types.IsTerminalStatus(s.status)
	if claimed {
		s.runID = runID
		s.claimOrder = append(s.claimOrder, runID)
		// A new execution starts from clean progress, like the SQL claim does.
		s.progress = 0
		s.speed = ""
		s.fps = 0
	}
	state := &types.JobRunState{
		GUID:            s.job.GUID,
		Status:          s.status,
		RunID:           types.NullString{NullString: sqlNullString(s.runID)},
		CancelRequested: s.cancelRequested,
	}
	hook := s.onClaim
	s.mu.Unlock()

	if claimed && hook != nil {
		hook(runID)
	}
	return state, claimed, nil
}

func (s *fakeStore) UpdateJobRunStatus(guid string, runID string, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID || types.IsTerminalStatus(s.status) {
		return types.ErrStaleRun
	}
	s.status = status
	s.statusLog = append(s.statusLog, status)
	return nil
}

func (s *fakeStore) UpdateEncodeProbeForRun(guid string, runID string, jsonString string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID || types.IsTerminalStatus(s.status) {
		return types.ErrStaleRun
	}
	s.probe = jsonString
	return nil
}

func (s *fakeStore) UpdateEncodeOptionsForRun(guid string, runID string, options string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID || types.IsTerminalStatus(s.status) {
		return types.ErrStaleRun
	}
	s.options = options
	return nil
}

func (s *fakeStore) UpdateEncodeProgressForRun(guid string, runID string, progress float64, speed string, fps float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accepted := s.runID == runID && !types.IsTerminalStatus(s.status) && !s.cancelRequested
	s.writeLog = append(s.writeLog, progressWrite{runID: runID, progress: progress, accepted: accepted})
	if !accepted {
		return types.ErrStaleRun
	}
	s.progress = progress
	s.speed = speed
	s.fps = fps
	return nil
}

func (s *fakeStore) UpdateTransferProgressForRun(guid string, runID string, progress float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accepted := s.runID == runID && !types.IsTerminalStatus(s.status) && !s.cancelRequested
	if !accepted {
		return types.ErrStaleRun
	}
	s.progress = progress
	return nil
}

func (s *fakeStore) FinishJobRun(guid string, runID string, status string) (string, error) {
	s.mu.Lock()
	hook := s.beforeFinish
	s.mu.Unlock()
	if hook != nil {
		hook()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID || types.IsTerminalStatus(s.status) {
		return s.status, types.ErrStaleRun
	}
	final := status
	if s.status == types.JobCancelled || s.cancelRequested {
		final = types.JobCancelled
	}
	s.status = final
	s.statusLog = append(s.statusLog, final)
	s.runID = ""
	s.cancelRequested = false
	return final, nil
}

// RestartJobRunByGUID mirrors the SQL restart: it clears the run binding so the
// previous execution can no longer write, resets progress and moves the job to
// 'restarting'.
func (s *fakeStore) RestartJobRunByGUID(guid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = types.JobRestarting
	s.statusLog = append(s.statusLog, types.JobRestarting)
	s.runID = ""
	s.cancelRequested = false
	s.progress = 0
	s.speed = ""
	s.fps = 0
	return true, nil
}

func (s *fakeStore) ReleaseJobRun(guid string, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID == runID {
		s.runID = ""
		s.cancelRequested = false
	}
	return nil
}

func (s *fakeStore) RequestJobCancelByGUID(guid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if types.IsTerminalStatus(s.status) {
		return false, nil
	}
	s.status = types.JobCancelled
	s.cancelRequested = true
	s.statusLog = append(s.statusLog, types.JobCancelled)
	return true, nil
}

// helpers used by tests to inspect state.
func (s *fakeStore) snapshot() (status string, runID string, progress float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.runID, s.progress
}

func (s *fakeStore) acceptedWritesFor(runID string) []progressWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []progressWrite
	for _, w := range s.writeLog {
		if w.runID == runID {
			out = append(out, w)
		}
	}
	return out
}

func (s *fakeStore) statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statusLog...)
}

func (s *fakeStore) claims() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.claimOrder...)
}

// sqlNullString converts a plain string to a valid sql.NullString.
func sqlNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

type fakeSettings struct {
	values map[string]string
}

func (f fakeSettings) GetSetting(key string) (*types.Setting, error) {
	v, ok := f.values[key]
	if !ok {
		return nil, errors.New("setting not found: " + key)
	}
	return &types.Setting{Value: v}, nil
}

func (f fakeSettings) GetSettings() []types.Setting { return nil }

type fakePresets struct{}

func (fakePresets) GetPresetByName(name string) (*types.Preset, error) {
	return &types.Preset{Name: name, Data: `{"raw":["-y"]}`, Output: "out.mp4"}, nil
}

type fakeProber struct {
	calls    int
	noFrames bool
	err      error
	inputs   []string
	mu       sync.Mutex
}

func (p *fakeProber) Run(ctx context.Context, input string) (*encoder.FFProbeResponse, error) {
	p.mu.Lock()
	p.calls++
	p.inputs = append(p.inputs, input)
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.err != nil {
		return nil, p.err
	}
	frames := "100"
	if p.noFrames {
		frames = ""
	}
	return &encoder.FFProbeResponse{Streams: []encoder.Stream{{NbFrames: frames}}}, nil
}

func (p *fakeProber) lastInput() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inputs) == 0 {
		return ""
	}
	return p.inputs[len(p.inputs)-1]
}

func (p *fakeProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// fakeTransfer lets tests control download/upload duration and observe the
// progress callback.
type fakeTransfer struct {
	mu sync.Mutex

	downloadStarted chan struct{}
	uploadStarted   chan struct{}

	// blockDownload/blockUpload makes Download/Upload wait for the context.
	blockDownload bool
	blockUpload   bool

	downloadErr error
	uploadErr   error

	downloads int
	uploads   int

	// reportProgress sends a progress update through the callback when set.
	reportProgress bool

	// uploadSawMissingFiles is set when Upload ran after the encoded files had
	// already been removed.
	uploadSawMissingFiles bool

	// dstDir is the directory Upload checks to detect premature cleanup.
	dstDir string
}

func newFakeTransfer() *fakeTransfer {
	return &fakeTransfer{
		downloadStarted: make(chan struct{}),
		uploadStarted:   make(chan struct{}),
	}
}

func (t *fakeTransfer) Download(ctx context.Context, job types.Job, report func(float64)) error {
	t.mu.Lock()
	t.downloads++
	t.mu.Unlock()
	closeOnce(t.downloadStarted)

	if t.blockDownload {
		<-ctx.Done()
		return ctx.Err()
	}
	if t.reportProgress && report != nil {
		report(50)
	}
	return t.downloadErr
}

func (t *fakeTransfer) Upload(ctx context.Context, job types.Job, report func(float64)) error {
	t.mu.Lock()
	t.uploads++
	if t.dstDir != "" {
		if _, err := os.Stat(t.dstDir); err != nil {
			t.uploadSawMissingFiles = true
		}
	}
	t.mu.Unlock()
	closeOnce(t.uploadStarted)

	if t.blockUpload {
		<-ctx.Done()
		return ctx.Err()
	}
	if t.reportProgress && report != nil {
		report(50)
	}
	return t.uploadErr
}

func (t *fakeTransfer) GetPresignedURL(job types.Job) (string, error) {
	return "https://example.invalid/presigned", nil
}

func (t *fakeTransfer) uploadsStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.uploads > 0
}

func (t *fakeTransfer) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downloads, t.uploads
}

// fakeEncoder simulates an FFmpeg process. It can block until cancelled, emit
// progress, and records whether Cancel/Done were called.
type fakeEncoder struct {
	mu sync.Mutex

	frame int
	fps   float64
	speed string

	cancelled bool
	done      bool
	runErr    error

	// block makes Run wait for the context to be cancelled.
	block bool
	// delay makes Run take some time before succeeding.
	delay time.Duration
	// onRun is invoked at the start of Run.
	onRun func()

	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func newFakeEncoder() *fakeEncoder {
	return &fakeEncoder{
		started: make(chan struct{}),
		release: make(chan struct{}),
		fps:     24,
		speed:   "1x",
	}
}

func (e *fakeEncoder) Run(ctx context.Context, input, output, data string) error {
	if e.onRun != nil {
		e.onRun()
	}
	e.startOnce.Do(func() { close(e.started) })

	if e.block {
		select {
		case <-ctx.Done():
			return encoder.ErrCancelled
		case <-e.release:
			return e.runErr
		}
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return encoder.ErrCancelled
		}
	}
	return e.runErr
}

func (e *fakeEncoder) ProgressSnapshot() encoder.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return encoder.Snapshot{Frame: e.frame, FPS: e.fps, Speed: e.speed}
}

func (e *fakeEncoder) Done() {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
}

func (e *fakeEncoder) wasStarted() bool {
	select {
	case <-e.started:
		return true
	default:
		return false
	}
}

// forceRelease unblocks a blocked fake encoder without cancelling the context,
// emulating a process that exits on its own.
func (e *fakeEncoder) forceRelease() {
	closeOnce(e.release)
}

func (e *fakeEncoder) setFrame(f int) {
	e.mu.Lock()
	e.frame = f
	e.mu.Unlock()
}

func (e *fakeEncoder) wasDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// fakeAlerter records sent alerts.
type fakeAlerter struct {
	mu   sync.Mutex
	sent int
}

func (a *fakeAlerter) Send(job types.Job) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent++
	return nil
}

func (a *fakeAlerter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sent
}

func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}
