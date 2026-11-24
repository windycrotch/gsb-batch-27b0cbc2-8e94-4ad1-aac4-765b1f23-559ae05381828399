package chime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- test doubles ----------

// fakeSound blocks in PlayOnce until it is released, so tests control exactly
// when a strike finishes. It records how many strikes run concurrently.
type fakeSound struct {
	rig *rig
}

func (s *fakeSound) PlayOnce(ctx context.Context) error {
	cur := s.rig.active.Add(1)
	for {
		max := s.rig.maxActive.Load()
		if cur <= max || s.rig.maxActive.CompareAndSwap(max, cur) {
			break
		}
	}
	defer s.rig.active.Add(-1)

	s.rig.starts <- struct{}{}

	select {
	case <-s.rig.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rig wires a fake Loader to the Scheduler.
type rig struct {
	loader *fakeLoader

	starts    chan struct{}
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32

	mu       sync.Mutex
	loads    int
	closes   int
	reports  []error
	reportCh chan error
}

func newRig() *rig {
	r := &rig{
		starts:   make(chan struct{}, 128),
		release:  make(chan struct{}, 128),
		reportCh: make(chan error, 128),
	}
	r.loader = &fakeLoader{rig: r}
	return r
}

func (r *rig) scheduler(gap time.Duration) *Scheduler {
	return New(Options{
		Loader: r.loader,
		Reporter: ReporterFunc(func(err error) {
			r.mu.Lock()
			r.reports = append(r.reports, err)
			r.mu.Unlock()
			select {
			case r.reportCh <- err:
			default:
			}
		}),
		Gap: gap,
	})
}

func (r *rig) reportCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reports)
}

func (r *rig) loadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads
}

type fakeLoader struct {
	rig *rig
	// failNext makes the next n Load calls fail.
	failures atomic.Int32
}

func (l *fakeLoader) Load() (Sound, error) {
	l.rig.mu.Lock()
	l.rig.loads++
	l.rig.mu.Unlock()

	if n := l.failures.Load(); n > 0 {
		l.failures.Add(-1)
		return nil, errors.New("audio device busy")
	}
	return &fakeSound{rig: l.rig}, nil
}

func (l *fakeLoader) Close() error {
	l.rig.mu.Lock()
	l.rig.closes++
	l.rig.mu.Unlock()
	return nil
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}

func quarterHour(hour int) time.Time {
	return time.Date(2026, 3, 1, hour, 15, 0, 0, time.UTC)
}

// ---------- strike-count rules ----------

func TestCountFollowsTwelveHourRule(t *testing.T) {
	cases := []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 12},
		{time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), 12},
		{time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC), 6},
		{time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC), 11},
		{time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 3, 1, 18, 0, 0, 0, time.UTC), 6},
		{time.Date(2026, 3, 1, 23, 0, 0, 0, time.UTC), 11},
		{time.Date(2026, 3, 1, 9, 15, 0, 0, time.UTC), 1},
		{time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC), 1},
		{time.Date(2026, 3, 1, 9, 45, 0, 0, time.UTC), 1},
	}
	for _, c := range cases {
		if got := Count(c.at); got != c.want {
			t.Errorf("Count(%s) = %d, want %d", c.at.Format("15:04"), got, c.want)
		}
	}
}

func TestIsChimeMinute(t *testing.T) {
	for m := 0; m < 60; m++ {
		at := time.Date(2026, 3, 1, 9, m, 0, 0, time.UTC)
		want := m == 0 || m == 15 || m == 30 || m == 45
		if got := IsChimeMinute(at); got != want {
			t.Errorf("IsChimeMinute(%02d) = %v, want %v", m, got, want)
		}
	}
}

// ---------- serialisation ----------

func TestTriggersNeverOverlap(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	var accepted int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Trigger(quarterHour(10)) {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	if accepted < 1 {
		t.Fatalf("no trigger was accepted")
	}

	// Release every strike that starts; with a one-slot queue this drains the
	// scheduler without letting two strikes run together.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-r.starts:
				r.release <- struct{}{}
			case <-time.After(150 * time.Millisecond):
				return
			}
		}
	}()
	<-done

	if max := r.maxActive.Load(); max > 1 {
		t.Fatalf("%d strikes ran concurrently, want at most 1", max)
	}
}

func TestQueueKeepsOnePendingTriggerAndDropsTheRest(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	if !s.Trigger(quarterHour(10)) {
		t.Fatalf("first trigger should be accepted")
	}
	awaitSignal(t, r.starts, "first chime to start")

	if !s.Trigger(quarterHour(11)) {
		t.Fatalf("second trigger should be queued while the first plays")
	}
	if s.Trigger(quarterHour(12)) {
		t.Fatalf("third trigger should be dropped while a trigger is already queued")
	}

	// Nothing else may start until the running chime finishes.
	assertNoSignal(t, r.starts, "concurrent chime")

	r.release <- struct{}{} // finish chime 1
	awaitSignal(t, r.starts, "queued chime to start")
	assertNoSignal(t, r.starts, "extra chime beyond the queued one")
	r.release <- struct{}{} // finish chime 2

	waitFor(t, "both sequences to complete", func() bool {
		return r.loadCount() == 2
	})
}

func TestSequencePlaysAllStrikesSequentially(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	const wantStrikes = 12 // midnight strikes twelve times
	if !s.Trigger(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("trigger was not accepted")
	}

	for i := 0; i < wantStrikes; i++ {
		awaitSignal(t, r.starts, "strike")
		if active := r.active.Load(); active != 1 {
			t.Fatalf("strike %d started with %d strikes active, want 1", i+1, active)
		}
		r.release <- struct{}{}
	}
	assertNoSignal(t, r.starts, "strike beyond the twelfth")
}

// TestNextStrikeWaitsForPlayerCompletion proves the sequence is driven by the
// player reporting completion rather than by an estimated duration.
func TestNextStrikeWaitsForPlayerCompletion(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	if !s.Trigger(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)) { // three strikes
		t.Fatalf("trigger was not accepted")
	}

	awaitSignal(t, r.starts, "first strike")
	// Hold the first strike longer than any plausible duration estimate.
	assertNoSignal(t, r.starts, "second strike before the first finished")
	time.Sleep(25 * time.Millisecond)
	assertNoSignal(t, r.starts, "second strike before the first finished")

	r.release <- struct{}{}
	awaitSignal(t, r.starts, "second strike after the first finished")

	r.release <- struct{}{}
	awaitSignal(t, r.starts, "third strike after the second finished")
	r.release <- struct{}{}

	waitFor(t, "sequence to finish", func() bool { return r.active.Load() == 0 })
}

// ---------- cancellation ----------

func TestShutdownInterruptsPlaybackPromptly(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !s.Trigger(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("trigger was not accepted")
	}
	awaitSignal(t, r.starts, "chime to start")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown took %v, want prompt cancellation", elapsed)
	}
	if active := r.active.Load(); active != 0 {
		t.Fatalf("%d strikes still active after Shutdown", active)
	}
	if s.Trigger(quarterHour(9)) {
		t.Fatalf("trigger was accepted after Shutdown")
	}
	r.mu.Lock()
	closes := r.closes
	r.mu.Unlock()
	if closes != 1 {
		t.Fatalf("loader closed %d times, want 1", closes)
	}
}

// TestShutdownReportsStuckPlayback covers a backend that ignores cancellation:
// Shutdown must still return within the deadline so the process can exit.
func TestShutdownReportsStuckPlayback(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)

	loader := &stubbornLoader{sound: &stubbornSound{stuck: stuck, started: make(chan struct{})}}
	s := New(Options{Loader: loader, Gap: -1})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.Trigger(quarterHour(9)) {
		t.Fatalf("trigger was not accepted")
	}
	awaitSignal(t, loader.sound.started, "chime to start")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Shutdown(ctx)
	if err == nil {
		t.Fatalf("Shutdown succeeded although playback ignored cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %v, want it bounded by the deadline", elapsed)
	}
}

type stubbornLoader struct{ sound *stubbornSound }

func (l *stubbornLoader) Load() (Sound, error) { return l.sound, nil }
func (l *stubbornLoader) Close() error         { return nil }

type stubbornSound struct {
	stuck   chan struct{}
	started chan struct{}
	once    sync.Once
}

func (s *stubbornSound) PlayOnce(context.Context) error {
	s.once.Do(func() { close(s.started) })
	<-s.stuck
	return nil
}

func TestShutdownAfterStartAndBeforeStart(t *testing.T) {
	notStarted := New(Options{Loader: newRig().loader})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := notStarted.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
	if notStarted.Trigger(time.Now()) {
		t.Fatalf("trigger accepted before Start")
	}

	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatalf("second Start succeeded, want error")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

func TestStartRequiresLoader(t *testing.T) {
	s := New(Options{})
	if err := s.Start(context.Background()); err == nil {
		t.Fatalf("Start without a loader succeeded, want error")
	}
}

// TestStartRefusesCancelledContext covers a quit that races with startup: no
// worker may be launched, and no goroutine may be left behind.
func TestStartRefusesCancelledContext(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start with a cancelled context = %v, want context.Canceled", err)
	}
	if s.Trigger(quarterHour(9)) {
		t.Fatalf("trigger accepted although no worker runs")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown after a refused Start: %v", err)
	}
}

func TestParentContextCancellationStopsScheduler(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown after parent cancel: %v", err)
	}
}

// ---------- resource failure and recovery ----------

// healthyReporter records both failures and recoveries.
type healthyReporter struct {
	mu      sync.Mutex
	errors  []error
	healthy int
	okCh    chan struct{}
}

func (r *healthyReporter) ReportError(err error) {
	r.mu.Lock()
	r.errors = append(r.errors, err)
	r.mu.Unlock()
}

func (r *healthyReporter) ReportHealthy() {
	r.mu.Lock()
	r.healthy++
	r.mu.Unlock()
	select {
	case r.okCh <- struct{}{}:
	default:
	}
}

func TestRecoveryReportsHealthyAfterSuccessfulSequence(t *testing.T) {
	r := newRig()
	r.loader.failures.Store(1)
	reporter := &healthyReporter{okCh: make(chan struct{}, 4)}
	s := New(Options{Loader: r.loader, Reporter: reporter, Gap: -1})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	// Failing load: an error is reported, no strike plays, no health notice.
	if !s.Trigger(quarterHour(10)) {
		t.Fatalf("trigger was not accepted")
	}
	waitFor(t, "failure to be reported", func() bool {
		reporter.mu.Lock()
		defer reporter.mu.Unlock()
		return len(reporter.errors) == 1
	})
	reporter.mu.Lock()
	healthyAfterFailure := reporter.healthy
	reporter.mu.Unlock()
	if healthyAfterFailure != 0 {
		t.Fatalf("health reported for a failed sequence")
	}

	// The resource works again: a chime plays and the sequence reports healthy.
	if !s.Trigger(quarterHour(11)) {
		t.Fatalf("second trigger was not accepted")
	}
	awaitSignal(t, r.starts, "strike after recovery")
	r.release <- struct{}{}
	select {
	case <-reporter.okCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("recovered sequence did not report healthy")
	}
}

func TestLoadFailureIsReportedAndDoesNotStopTheScheduler(t *testing.T) {
	r := newRig()
	r.loader.failures.Store(1)
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	if !s.Trigger(quarterHour(10)) {
		t.Fatalf("trigger was not accepted")
	}
	select {
	case err := <-r.reportCh:
		if err == nil {
			t.Fatalf("reported a nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("load failure was not reported")
	}
	assertNoSignal(t, r.starts, "strike after a failed load")

	// The resource is available again: the next trigger must play.
	if !s.Trigger(quarterHour(11)) {
		t.Fatalf("second trigger was not accepted")
	}
	awaitSignal(t, r.starts, "strike after recovery")
	r.release <- struct{}{}
}

func TestFailingLoadKeepsRetryingOnEveryTrigger(t *testing.T) {
	r := newRig()
	r.loader.failures.Store(3)
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	for i := 0; i < 3; i++ {
		if !s.Trigger(quarterHour(10)) {
			t.Fatalf("trigger %d was not accepted", i)
		}
		select {
		case <-r.reportCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("failure %d was not reported", i)
		}
	}
	if r.loadCount() != 3 {
		t.Fatalf("loader called %d times, want 3", r.loadCount())
	}

	if !s.Trigger(quarterHour(10)) {
		t.Fatalf("trigger after recovery was not accepted")
	}
	awaitSignal(t, r.starts, "strike once the resource is available")
	r.release <- struct{}{}
	if r.reportCount() != 3 {
		t.Fatalf("reported %d failures, want 3", r.reportCount())
	}
}

func TestPlaybackErrorIsReportedAndSequenceStops(t *testing.T) {
	loader := &erroringLoader{reports: make(chan error, 8)}
	s := New(Options{
		Loader:   loader,
		Reporter: ReporterFunc(func(err error) { loader.reports <- err }),
		Gap:      -1,
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	if !s.Trigger(time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("trigger was not accepted")
	}
	select {
	case err := <-loader.reports:
		if err == nil {
			t.Fatalf("reported a nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("playback error was not reported")
	}
	// A failed playback must not be retried for the remaining strikes.
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("PlayOnce called %d times, want 1", got)
	}
}

type erroringLoader struct {
	reports chan error
	calls   atomic.Int32
}

func (l *erroringLoader) Load() (Sound, error) { return &erroringSound{l: l}, nil }
func (l *erroringLoader) Close() error         { return nil }

type erroringSound struct{ l *erroringLoader }

func (s *erroringSound) PlayOnce(context.Context) error {
	s.l.calls.Add(1)
	return errors.New("speaker device lost")
}

// ---------- clock wiring ----------

func TestRunClockTriggersOnlyOnChimeMinutes(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunClock(ctx, ticks, s)

	drain := func() {
		r.release <- struct{}{}
		for {
			select {
			case <-r.starts:
				r.release <- struct{}{}
			case <-time.After(30 * time.Millisecond):
				return
			}
		}
	}

	ticks <- time.Date(2026, 3, 1, 9, 14, 0, 0, time.UTC)
	assertNoSignal(t, r.starts, "chime at :14")

	ticks <- time.Date(2026, 3, 1, 9, 15, 0, 0, time.UTC)
	awaitSignal(t, r.starts, "chime at :15")
	drain()

	ticks <- time.Date(2026, 3, 1, 9, 16, 0, 0, time.UTC)
	assertNoSignal(t, r.starts, "chime at :16")

	ticks <- time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	awaitSignal(t, r.starts, "chime at :30")
	drain()
}

func TestRunClockReturnsWhenTicksCloseOrContextEnds(t *testing.T) {
	r := newRig()
	s := r.scheduler(-1)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		RunClock(context.Background(), ticks, s)
		close(done)
	}()
	close(ticks)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunClock did not return when ticks closed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		RunClock(ctx, make(chan time.Time), s)
		close(done2)
	}()
	cancel()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunClock did not return when the context ended")
	}
}

func TestTickEveryFiresAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := TickEvery(ctx, 10*time.Millisecond)

	for i := 0; i < 3; i++ {
		select {
		case tick := <-ticks:
			// The value names the boundary it belongs to, which is what the
			// strike count is derived from.
			if tick.UnixNano()%(int64(10*time.Millisecond)) != 0 {
				t.Fatalf("tick %v is not aligned to the interval", tick)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no tick %d", i+1)
		}
	}

	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ticks:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("tick channel was not closed after cancellation")
		}
	}
}

func TestTickEveryWithNonPositiveIntervalIsClosed(t *testing.T) {
	ticks := TickEvery(context.Background(), 0)
	select {
	case _, ok := <-ticks:
		if ok {
			t.Fatalf("unexpected tick")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel was not closed")
	}
}
