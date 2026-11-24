// Package chime contains the scheduling engine of the City Hall clock.
//
// The engine is deliberately free of any real audio or GUI dependency: playback
// is provided by a Loader that returns a Sound, so scheduling, cancellation and
// resource-failure recovery can be tested without a sound card or an MP3 file.
package chime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// DefaultChimeGap is the pause between two consecutive strikes of the same
	// chime sequence.
	DefaultChimeGap = 500 * time.Millisecond

	// requestQueueDepth is the number of triggers that may wait while a chime
	// sequence is already playing. One pending trigger is enough to cover a
	// trigger that arrives a moment late; anything beyond that is dropped so a
	// backlog of stale chimes can never build up.
	requestQueueDepth = 1
)

// Sound is a single chime that can be played.
//
// PlayOnce must block until the sound has really finished playing (not until an
// estimated duration has elapsed) and must return promptly with ctx.Err() when
// ctx is cancelled. PlayOnce is never called concurrently by a Scheduler.
type Sound interface {
	PlayOnce(ctx context.Context) error
}

// Loader loads the chime sound.
//
// Load is called once before every triggered chime sequence, so a resource that
// was missing, unreadable or otherwise unavailable can recover on a later
// trigger instead of failing permanently. Implementations must be safe to call
// from the scheduler goroutine only.
type Loader interface {
	Load() (Sound, error)
	// Close releases whatever Load acquired. It must be safe to call more than
	// once and when Load was never called successfully.
	Close() error
}

// Reporter receives human readable failure notices (missing audio, missing
// icon, playback errors). Implementations may be called from any goroutine.
type Reporter interface {
	ReportError(err error)
}

// HealthReporter is an optional extension of Reporter. When the configured
// Reporter implements it, ReportHealthy is called after a chime sequence has
// played to completion, which lets the UI clear a stale failure notice once the
// resource works again.
type HealthReporter interface {
	ReportHealthy()
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(error)

// ReportError calls f.
func (f ReporterFunc) ReportError(err error) { f(err) }

// DiscardReporter throws every report away.
type DiscardReporter struct{}

// ReportError implements Reporter.
func (DiscardReporter) ReportError(error) {}

// Options configures a Scheduler.
type Options struct {
	// Loader provides the sound for each trigger. Required.
	Loader Loader
	// Reporter receives failures. nil means failures are discarded.
	Reporter Reporter
	// Gap is the pause between strikes of a sequence. 0 selects
	// DefaultChimeGap; a negative value disables the pause (useful in tests).
	Gap time.Duration
}

// Count returns how many times the chime strikes for time t.
//
// On the hour the number follows a 12 hour clock: 00:00 and 12:00 strike twelve
// times, 13:00 strikes once, and so on. Quarter hours (15, 30 and 45 minutes)
// strike once. The rule is intentionally unchanged from the original program.
func Count(t time.Time) int {
	if t.Minute() != 0 {
		return 1
	}
	n := t.Hour() % 12
	if n == 0 {
		n = 12
	}
	return n
}

// IsChimeMinute reports whether t is one of the minutes that trigger a chime.
func IsChimeMinute(t time.Time) bool {
	switch t.Minute() {
	case 0, 15, 30, 45:
		return true
	default:
		return false
	}
}

// Scheduler serialises chime playback.
//
// A single worker goroutine owns playback, therefore two chimes can never sound
// at the same time. A trigger that arrives while a sequence is playing is
// queued in a one-slot buffer; further triggers are dropped. This is the single,
// uniform policy used for every trigger.
type Scheduler struct {
	loader   Loader
	reporter Reporter
	gap      time.Duration

	requests chan time.Time

	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	finished chan struct{}
}

// New creates a Scheduler. Start must be called before it will accept triggers.
func New(opts Options) *Scheduler {
	reporter := opts.Reporter
	if reporter == nil {
		reporter = DiscardReporter{}
	}
	gap := opts.Gap
	switch {
	case gap == 0:
		gap = DefaultChimeGap
	case gap < 0:
		gap = 0
	}
	return &Scheduler{
		loader:   opts.Loader,
		reporter: reporter,
		gap:      gap,
		requests: make(chan time.Time, requestQueueDepth),
		finished: make(chan struct{}),
	}
}

// Start launches the playback worker. The worker stops when ctx is cancelled or
// when Shutdown is called.
//
// Start returns an error without launching anything when ctx is already
// cancelled, so a quit that races with startup cannot leave a worker behind.
func (s *Scheduler) Start(ctx context.Context) error {
	if s.loader == nil {
		return errors.New("chime: scheduler started without a loader")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("chime: not starting scheduler: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("chime: scheduler already started")
	}
	if s.cancel != nil {
		return errors.New("chime: scheduler already shut down")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.started = true
	go func() {
		defer close(s.finished)
		s.run(runCtx)
	}()
	return nil
}

// Trigger asks the scheduler to play the chime for time at.
//
// It never blocks. It returns true when the trigger was accepted (that is, the
// scheduler is idle or has room in its single pending slot) and false when the
// trigger was dropped because the scheduler is stopped or already busy.
func (s *Scheduler) Trigger(at time.Time) bool {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return false
	}
	select {
	case s.requests <- at:
		return true
	default:
		return false
	}
}

// Shutdown cancels any playback in progress and waits for the worker goroutine
// to finish. It returns ctx.Err() wrapped in an error if the worker does not
// stop before ctx expires, so a stuck audio backend cannot hang process exit.
//
// Shutdown is safe to call more than once; every call waits for the same
// worker.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.started = false
	s.mu.Unlock()

	if cancel == nil {
		return nil // never started
	}
	cancel()

	select {
	case <-s.finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("chime: playback did not stop in time: %w", ctx.Err())
	}
}

// run is the only goroutine that plays sound.
func (s *Scheduler) run(ctx context.Context) {
	defer func() {
		// No worker is left to consume triggers, whatever stopped it.
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()

		if err := s.loader.Close(); err != nil {
			s.report(fmt.Errorf("chime: releasing audio resources: %w", err))
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-s.requests:
			s.playSequence(ctx, at)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// playSequence plays every strike of one chime sequence. It returns early when
// ctx is cancelled or when playback (or loading) fails.
func (s *Scheduler) playSequence(ctx context.Context, at time.Time) {
	if ctx.Err() != nil {
		return
	}

	sound, err := s.loader.Load()
	if err != nil {
		s.report(fmt.Errorf("chime at %s: cannot load sound: %w", at.Format("15:04"), err))
		return
	}

	strikes := Count(at)
	for i := 0; i < strikes; i++ {
		if err := sound.PlayOnce(ctx); err != nil {
			if !isCancellation(err) {
				s.report(fmt.Errorf("chime at %s: playback failed: %w", at.Format("15:04"), err))
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if i < strikes-1 && !sleepCtx(ctx, s.gap) {
			return
		}
	}

	// A complete sequence means the audio resource and the device both work.
	s.reportHealthy()
}

func (s *Scheduler) report(err error) {
	s.reporter.ReportError(err)
}

func (s *Scheduler) reportHealthy() {
	if h, ok := s.reporter.(HealthReporter); ok {
		h.ReportHealthy()
	}
}

// RunClock reads ticks and triggers the scheduler on chime minutes. It returns
// when ctx is done or the tick channel is closed. Ticks are supplied by the
// caller so tests can drive the clock without waiting for real wall time.
func RunClock(ctx context.Context, ticks <-chan time.Time, s *Scheduler) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ticks:
			if !ok {
				return
			}
			if IsChimeMinute(t) {
				s.Trigger(t)
			}
		}
	}
}

// TickEvery returns a channel that delivers one tick per d, aligned to
// multiples of d since the Unix epoch (so a minute tick fires at :00 seconds).
// The value sent is the scheduled boundary, not the (slightly later) instant
// the timer fired, so a tick always names the minute it belongs to.
// The channel is closed when ctx is done.
func TickEvery(ctx context.Context, d time.Duration) <-chan time.Time {
	ch := make(chan time.Time)
	if d <= 0 {
		close(ch)
		return ch
	}
	go func() {
		defer close(ch)
		next := time.Now().Truncate(d).Add(d)
		timer := time.NewTimer(time.Until(next))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				select {
				case ch <- next:
				case <-ctx.Done():
					return
				}
				next = next.Add(d)
				if !next.After(time.Now()) {
					// We fell behind (suspend, long playback); resynchronise
					// instead of firing a burst of catch-up ticks.
					next = time.Now().Truncate(d).Add(d)
				}
				timer.Reset(time.Until(next))
			}
		}
	}()
	return ch
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
