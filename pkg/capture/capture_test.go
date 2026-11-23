package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// callLog records cleanup operations in the order they happened.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	calls := make([]string, len(l.calls))
	copy(calls, l.calls)
	return calls
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeEventSource is a controllable replacement for the audio ready event.
type fakeEventSource struct {
	mu        sync.Mutex
	waits     int
	active    int
	maxActive int
	closed    int
	closeErr  error

	block  chan struct{}
	onWait func(n int)
	errAt  int
	err    error
	log    *callLog
}

func (f *fakeEventSource) Wait(ctx context.Context) error {
	f.mu.Lock()
	f.waits++
	n := f.waits
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.onWait != nil {
		f.onWait(n)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.err != nil && f.errAt > 0 && n >= f.errAt {
		return f.err
	}
	return nil
}

func (f *fakeEventSource) Close() error {
	f.mu.Lock()
	f.closed++
	err := f.closeErr
	f.mu.Unlock()
	if f.log != nil {
		f.log.add("close:event")
	}
	return err
}

func (f *fakeEventSource) stats() (waits, active, maxActive, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits, f.active, f.maxActive, f.closed
}

// fakeBufferSource is a controllable replacement for IAudioCaptureClient.
type fakeBufferSource struct {
	mu         sync.Mutex
	data       []byte
	frames     uint32
	zeroFrames int

	acquires  int
	successes int
	releases  int
	active    int
	maxActive int
	released  []uint32

	acquireErr   error
	acquireErrAt int
	noDataAt     map[int]bool
	releaseErr   error
	releaseErrAt int
	onAcquire    func(n int)
}

func (b *fakeBufferSource) Acquire() (uint32, []byte, error) {
	b.mu.Lock()
	b.acquires++
	n := b.acquires
	b.mu.Unlock()

	if b.onAcquire != nil {
		b.onAcquire(n)
	}
	if b.acquireErr != nil && b.acquireErrAt == n {
		return 0, nil, b.acquireErr
	}
	if b.noDataAt[n] {
		return 0, nil, ErrNoData
	}

	b.mu.Lock()
	b.successes++
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	zero := n <= b.zeroFrames
	b.mu.Unlock()

	if zero {
		return 0, nil, nil
	}
	return b.frames, append([]byte(nil), b.data...), nil
}

func (b *fakeBufferSource) Release(frames uint32) error {
	b.mu.Lock()
	b.releases++
	b.active--
	b.released = append(b.released, frames)
	err := b.releaseErr
	at := b.releaseErrAt
	releases := b.releases
	b.mu.Unlock()

	if err != nil && at == releases {
		return err
	}
	return nil
}

func (b *fakeBufferSource) stats() (acquires, successes, releases, active, maxActive int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquires, b.successes, b.releases, b.active, b.maxActive
}

type fakeStopper struct {
	mu    sync.Mutex
	name  string
	calls int
	err   error
	log   *callLog
}

func (s *fakeStopper) Stop() error {
	s.mu.Lock()
	s.calls++
	err := s.err
	s.mu.Unlock()
	if s.log != nil {
		s.log.add("stop:" + s.name)
	}
	return err
}

func (s *fakeStopper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

func newTestSession(log *callLog, duration time.Duration, bytesPerSecond uint32, output *bytes.Buffer) (*Session, *fakeEventSource, *fakeBufferSource, *fakeStopper, *fakeStopper) {
	events := &fakeEventSource{log: log}
	buffers := &fakeBufferSource{data: []byte{0x01, 0x02, 0x03, 0x04}}
	captureStopper := &fakeStopper{name: "capture", log: log}
	renderStopper := &fakeStopper{name: "render", log: log}

	session := &Session{
		Events:         events,
		Buffers:        buffers,
		Duration:       duration,
		BytesPerSecond: bytesPerSecond,
		Stoppers:       []Stopper{captureStopper, renderStopper},
	}
	if output != nil {
		session.Output = io.Writer(output)
	}
	return session, events, buffers, captureStopper, renderStopper
}

func TestRunUntilDuration(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, cac, rac := newTestSession(log, time.Second, 10, out)

	if err := session.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	want := []byte{
		0x01, 0x02, 0x03, 0x04,
		0x01, 0x02, 0x03, 0x04,
		0x01, 0x02, 0x03, 0x04,
	}
	if got := out.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("output = %v, want %v", got, want)
	}

	waits, active, maxActive, closed := events.stats()
	if waits != 3 {
		t.Errorf("event waits = %d, want 3", waits)
	}
	if active != 0 {
		t.Errorf("active waiters after Run = %d, want 0", active)
	}
	if maxActive != 1 {
		t.Errorf("max active waiters = %d, want 1", maxActive)
	}
	if closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}

	acquires, successes, releases, activeBuffers, maxActiveBuffers := buffers.stats()
	if acquires != 3 || successes != 3 {
		t.Errorf("acquires/successes = %d/%d, want 3/3", acquires, successes)
	}
	if releases != successes {
		t.Errorf("releases = %d, want %d (same as successful acquires)", releases, successes)
	}
	if activeBuffers != 0 || maxActiveBuffers != 1 {
		t.Errorf("active buffers = %d (max %d), want 0 (max 1)", activeBuffers, maxActiveBuffers)
	}

	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	wantOrder := []string{"stop:capture", "stop:render", "close:event"}
	if got := log.snapshot(); !equalStrings(got, wantOrder) {
		t.Errorf("cleanup order = %v, want %v", got, wantOrder)
	}
}

func TestRunLongCaptureDoesNotAccumulateWaiters(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	// 100 bytes/s for 10 seconds means 250 four-byte chunks.
	session, events, buffers, _, _ := newTestSession(log, 10*time.Second, 100, out)

	if err := session.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	waits, active, maxActive, _ := events.stats()
	if waits != 250 {
		t.Errorf("event waits = %d, want 250", waits)
	}
	if active != 0 {
		t.Errorf("active waiters after Run = %d, want 0", active)
	}
	if maxActive != 1 {
		t.Errorf("max active waiters = %d, want 1; waiters must not accumulate", maxActive)
	}

	acquires, successes, releases, _, _ := buffers.stats()
	if acquires != 250 || successes != 250 {
		t.Errorf("acquires/successes = %d/%d, want 250/250", acquires, successes)
	}
	if releases != successes {
		t.Errorf("releases = %d, want %d", releases, successes)
	}
	if out.Len() != 1000 {
		t.Errorf("output length = %d, want 1000", out.Len())
	}
}

func TestRunInterruptWritesOutputAndReleasesBuffers(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, cac, rac := newTestSession(log, 0, 100, out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events.onWait = func(n int) {
		if n == 3 {
			cancel()
		}
	}

	if err := session.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil after interrupt", err)
	}

	if got, want := out.Len(), 8; got != want {
		t.Errorf("output length = %d, want %d (buffers captured before interrupt must be kept)", got, want)
	}

	acquires, successes, releases, activeBuffers, _ := buffers.stats()
	if acquires != 2 || successes != 2 {
		t.Errorf("acquires/successes = %d/%d, want 2/2", acquires, successes)
	}
	if releases != successes || activeBuffers != 0 {
		t.Errorf("releases = %d, active = %d; want %d released and none active", releases, activeBuffers, successes)
	}

	waits, active, maxActive, closed := events.stats()
	if waits != 3 {
		t.Errorf("event waits = %d, want 3", waits)
	}
	if active != 0 || maxActive != 1 {
		t.Errorf("active/max waiters = %d/%d, want 0/1", active, maxActive)
	}
	if closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	wantOrder := []string{"stop:capture", "stop:render", "close:event"}
	if got := log.snapshot(); !equalStrings(got, wantOrder) {
		t.Errorf("cleanup order = %v, want %v", got, wantOrder)
	}
}

func TestRunDurationAndInterruptFinishOnce(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	// 8 bytes/s for 1 second is two four-byte chunks.
	session, events, buffers, cac, rac := newTestSession(log, time.Second, 8, out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffers.onAcquire = func(n int) {
		if n == 2 {
			// The duration is reached by this very chunk, and the interrupt
			// arrives before the loop gets a chance to check the duration.
			cancel()
		}
	}

	if err := session.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if got, want := out.Len(), 8; got != want {
		t.Errorf("output length = %d, want %d", got, want)
	}
	acquires, successes, releases, _, _ := buffers.stats()
	if acquires != 2 || successes != 2 || releases != successes {
		t.Errorf("acquires/successes/releases = %d/%d/%d, want 2/2/2", acquires, successes, releases)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want exactly 1 each", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunEventWaitErrorCleansUp(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, cac, rac := newTestSession(log, 0, 100, out)

	waitErr := errors.New("event wait failed")
	events.err = waitErr
	events.errAt = 2

	err := session.Run(context.Background())
	if !errors.Is(err, waitErr) {
		t.Fatalf("Run() = %v, want %v", err, waitErr)
	}

	acquires, successes, releases, activeBuffers, _ := buffers.stats()
	if acquires != 1 || successes != 1 || releases != 1 || activeBuffers != 0 {
		t.Errorf("acquires/successes/releases/active = %d/%d/%d/%d, want 1/1/1/0", acquires, successes, releases, activeBuffers)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunAcquireNoDataRetriesWithoutRelease(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, _, _ := newTestSession(log, 0, 100, out)

	// The first three waits produce no data, so no buffer is acquired or
	// released; the fourth does.
	buffers.noDataAt = map[int]bool{1: true, 2: true, 3: true}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffers.onAcquire = func(n int) {
		if n == 4 {
			cancel()
		}
	}

	if err := session.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil (ErrNoData must not fail the session)", err)
	}

	acquires, successes, releases, _, _ := buffers.stats()
	if acquires != 4 || successes != 1 {
		t.Errorf("acquires/successes = %d/%d, want 4/1", acquires, successes)
	}
	if releases != successes {
		t.Errorf("releases = %d, want %d (ErrNoData acquires must not be released)", releases, successes)
	}
	if got, want := out.Len(), 4; got != want {
		t.Errorf("output length = %d, want %d", got, want)
	}
	waits, _, _, _ := events.stats()
	if waits != 4 {
		t.Errorf("event waits = %d, want 4", waits)
	}
}

func TestRunAcquireErrorCleansUp(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, cac, rac := newTestSession(log, 0, 100, out)

	acquireErr := errors.New("acquire failed")
	buffers.acquireErr = acquireErr
	buffers.acquireErrAt = 2

	err := session.Run(context.Background())
	if !errors.Is(err, acquireErr) {
		t.Fatalf("Run() = %v, want %v", err, acquireErr)
	}

	acquires, successes, releases, activeBuffers, _ := buffers.stats()
	if acquires != 2 || successes != 1 {
		t.Errorf("acquires/successes = %d/%d, want 2/1", acquires, successes)
	}
	if releases != successes || activeBuffers != 0 {
		t.Errorf("releases = %d, active = %d, want %d released and none active", releases, activeBuffers, successes)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunReleaseErrorStillCleansUp(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, cac, rac := newTestSession(log, 0, 100, out)

	releaseErr := errors.New("release failed")
	buffers.releaseErr = releaseErr
	buffers.releaseErrAt = 1

	err := session.Run(context.Background())
	if !errors.Is(err, releaseErr) {
		t.Fatalf("Run() = %v, want %v", err, releaseErr)
	}

	_, successes, releases, _, _ := buffers.stats()
	if releases != successes {
		t.Errorf("releases = %d, want %d", releases, successes)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunStopErrorKeepsRemainingCleanupAndReturnsFirstError(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, _, cac, rac := newTestSession(log, 0, 100, out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stopErr := errors.New("stop failed")
	cac.err = stopErr

	err := session.Run(ctx)
	if !errors.Is(err, stopErr) {
		t.Fatalf("Run() = %v, want the first stop error %v", err, stopErr)
	}

	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1 (a failing stop must not skip later cleanup)", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
	wantOrder := []string{"stop:capture", "stop:render", "close:event"}
	if got := log.snapshot(); !equalStrings(got, wantOrder) {
		t.Errorf("cleanup order = %v, want %v", got, wantOrder)
	}
}

func TestRunReturnsCauseInsteadOfStopError(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, _, cac, rac := newTestSession(log, 0, 100, out)

	causeErr := errors.New("wait failed")
	events.err = causeErr
	events.errAt = 1
	cac.err = errors.New("stop failed")

	err := session.Run(context.Background())
	if !errors.Is(err, causeErr) {
		t.Fatalf("Run() = %v, want the original failure %v", err, causeErr)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
}

func TestRunZeroFrameAcquireIsReleased(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, _, buffers, _, _ := newTestSession(log, time.Second, 10, out)
	buffers.data = []byte{0x05, 0x06, 0x07, 0x08, 0x09}
	buffers.frames = 5
	buffers.zeroFrames = 3

	if err := session.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	acquires, successes, releases, activeBuffers, _ := buffers.stats()
	if acquires != 5 || successes != 5 {
		t.Errorf("acquires/successes = %d/%d, want 5/5", acquires, successes)
	}
	if releases != successes || activeBuffers != 0 {
		t.Errorf("releases = %d, active = %d; want %d released and none active", releases, activeBuffers, successes)
	}
	for i, frames := range buffers.released {
		if i < 3 && frames != 0 {
			t.Errorf("release[%d] = %d frames, want 0", i, frames)
		}
	}
	if out.Len() != 10 {
		t.Errorf("output length = %d, want 10", out.Len())
	}
}

func TestRunWriteErrorStillReleasesBuffer(t *testing.T) {
	log := &callLog{}
	writeErr := errors.New("write failed")
	session, _, buffers, cac, rac := newTestSession(log, 0, 100, nil)
	session.Output = failingWriter{err: writeErr}

	err := session.Run(context.Background())
	if !errors.Is(err, writeErr) {
		t.Fatalf("Run() = %v, want %v", err, writeErr)
	}

	_, successes, releases, _, _ := buffers.stats()
	if successes != 1 || releases != 1 {
		t.Errorf("successes/releases = %d/%d, want 1/1", successes, releases)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
}

func TestRunFinishesOnlyOnce(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, _, cac, rac := newTestSession(log, 0, 100, out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := session.Run(ctx); err != nil {
		t.Fatalf("first Run() = %v, want nil", err)
	}
	if err := session.Run(ctx); err != nil {
		t.Fatalf("second Run() = %v, want nil", err)
	}

	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunInterruptsBlockedWait(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, buffers, _, _ := newTestSession(log, 0, 100, out)
	events.block = make(chan struct{}) // never signaled

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- session.Run(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	_, active, maxActive, closed := events.stats()
	if active != 0 || maxActive != 1 {
		t.Errorf("active/max waiters = %d/%d, want 0/1", active, maxActive)
	}
	if closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
	if acquires, _, releases, _, _ := buffers.stats(); acquires != 0 || releases != 0 {
		t.Errorf("acquires/releases = %d/%d, want 0/0", acquires, releases)
	}
}

func TestRunReturnsEventCloseErrorAfterSuccessfulStops(t *testing.T) {
	log := &callLog{}
	out := new(bytes.Buffer)
	session, events, _, cac, rac := newTestSession(log, 0, 100, out)

	closeErr := errors.New("close failed")
	events.closeErr = closeErr

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := session.Run(ctx)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Run() = %v, want %v", err, closeErr)
	}
	if cac.count() != 1 || rac.count() != 1 {
		t.Errorf("stop calls = %d/%d, want 1/1", cac.count(), rac.count())
	}
	if _, _, _, closed := events.stats(); closed != 1 {
		t.Errorf("event close calls = %d, want 1", closed)
	}
}

func TestRunWithoutOutputStillAccountsDuration(t *testing.T) {
	log := &callLog{}
	session, _, buffers, _, _ := newTestSession(log, time.Second, 10, nil)
	session.Output = nil

	if err := session.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	acquires, successes, releases, _, _ := buffers.stats()
	if acquires != 3 || successes != 3 || releases != 3 {
		t.Errorf("acquires/successes/releases = %d/%d/%d, want 3/3/3", acquires, successes, releases)
	}
}

func TestRunRejectsIncompleteConfiguration(t *testing.T) {
	ctx := context.Background()

	if err := (&Session{}).Run(ctx); err == nil {
		t.Error("Run() without an event source = nil, want error")
	}
	if err := (&Session{Events: &fakeEventSource{}}).Run(ctx); err == nil {
		t.Error("Run() without a buffer source = nil, want error")
	}
	if err := (&Session{
		Events:   &fakeEventSource{},
		Buffers:  &fakeBufferSource{},
		Duration: time.Second,
	}).Run(ctx); err == nil {
		t.Error("Run() with a duration but no byte rate = nil, want error")
	}
}
