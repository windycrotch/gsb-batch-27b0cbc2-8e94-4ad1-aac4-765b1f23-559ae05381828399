package playback

import (
	"context"
	"sync"

	"github.com/sqweek/sqribe/log"

	. "github.com/sqweek/sqribe/core/types"
)

// Player runs at most one playback session at a time and serialises
// Start/Stop/SetRange calls so they are safe to invoke from any goroutine.
type Player struct {
	mu      sync.Mutex
	session *session
	src     SampleSource
	sink    AudioSink
	synth   Synth
	events  EventSource
}

func NewPlayer(src SampleSource, sink AudioSink, synth Synth, events EventSource) *Player {
	return &Player{
		src:    src,
		sink:   sink,
		synth:  synth,
		events: events,
	}
}

// State reports whether a session is currently active.
func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return StateStopped
	}
	return p.session.state()
}

// State mirrors the legacy three-state model for callers that display it.
type State int

const (
	StateStopped State = iota
	StatePlaying
	StateStopping
)

/* Start launches a new session over rng, beginning at startPos (clamped into
** the range). If a session is already running it is fully shut down first, so
** rapid start/stop toggles can never overlap two sessions. */
func (p *Player) Start(rng Range, startPos FrameN, loop bool) error {
	if !rng.valid() {
		return ErrEmptyRange
	}
	p.mu.Lock()
	prev := p.session
	p.mu.Unlock()
	if prev != nil {
		prev.stopAndWait()
	}
	sess, err := newSession(p, rng, startPos, loop)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.session = sess
	p.mu.Unlock()
	return sess.run()
}

/* Stop requests termination and blocks until every goroutine has exited and the
** sink is stopped. It is a no-op when nothing is playing, and always leaves the
** player ready to Start again. */
func (p *Player) Stop() {
	p.mu.Lock()
	sess := p.session
	p.session = nil
	p.mu.Unlock()
	if sess != nil {
		sess.stopAndWait()
	}
}

/* SetRange swaps the active loop range while playing. The prefetch goroutine
** picks up the new bounds at its next block, re-clamping its position so no
** out-of-bounds read can occur. A no-op when stopped. */
func (p *Player) SetRange(rng Range) bool {
	if !rng.valid() {
		return false
	}
	p.mu.Lock()
	sess := p.session
	p.mu.Unlock()
	if sess == nil {
		return false
	}
	return sess.setRange(rng)
}

/* NotifyChange records that the score changed (beats and/or notes). Messages
** are coalesced by the feeder, so a burst of edits costs one rebuild. Never
** blocks the caller. */
func (p *Player) NotifyChange(c Change) {
	p.mu.Lock()
	sess := p.session
	p.mu.Unlock()
	if sess == nil || c.empty() {
		return
	}
	sess.notify(c)
}

// session holds all per-playback coordination primitives.
type session struct {
	player *Player

	// lifecycle: cancelling ctx unblocks every source wait; prefetchDone
	// closes when the prefetch goroutine exits, feederDone after full cleanup.
	ctx    context.Context
	cancel context.CancelFunc

	stateMu  sync.RWMutex
	stopping bool

	rangeMu sync.RWMutex
	rng     Range
	loop    bool

	// rangeCh carries at most one pending range update (latest-wins).
	rangeCh chan Range
	// changeCh carries coalesced score edits; buffered so NotifyChange never blocks.
	changeCh chan Change

	// samples is the prefetch -> feeder handoff queue.
	samples chan block

	// prefetchDone closes once the prefetch goroutine has fully exited.
	prefetchDone chan struct{}
	// doneCh is closed by the feeder when it exits; prefetch drains against it.
	feederDone chan struct{}

	peakMu     sync.Mutex
	midiPeak   float64
	wavePeak   float64

	startPos FrameN
	err      error
	errOnce  sync.Once
}

// block is one prefetch unit plus the bookkeeping the feeder needs to detect
// wraps and associate events.
type block struct {
	// frame is the absolute waveform frame of the first sample in buf.
	frame FrameN
	// rng is the loop range in effect when the block was produced.
	rng Range
	// wrapped is set when this block begins at the range start following a
	// wrap, which is the feeder's cue to replay events from the top.
	wrapped bool
	// tail marks the final block of a non-looping session.
	tail bool
	buf  []int16
}

func newSession(p *Player, rng Range, startPos FrameN, loop bool) (*session, error) {
	if startPos < rng.Min || startPos > rng.Max {
		startPos = rng.Min
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		player:       p,
		ctx:          ctx,
		cancel:       cancel,
		rng:          rng,
		loop:         loop,
		rangeCh:      make(chan Range, 1),
		changeCh:     make(chan Change, 16),
		samples:      make(chan block, 25),
		prefetchDone: make(chan struct{}),
		feederDone:   make(chan struct{}),
		startPos:     startPos,
	}
	return s, nil
}

func (s *session) state() State {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.stopping {
		return StateStopping
	}
	return StatePlaying
}

func (s *session) markStopping() {
	s.stateMu.Lock()
	s.stopping = true
	s.stateMu.Unlock()
}

func (s *session) setRange(rng Range) bool {
	select {
	case s.rangeCh <- rng:
		return true
	default:
		// an update is already queued; replace it with the latest
		select {
		case <-s.rangeCh:
		default:
		}
		select {
		case s.rangeCh <- rng:
			return true
		default:
			return false
		}
	}
}

func (s *session) notify(c Change) {
	select {
	case s.changeCh <- c:
	default:
		// queue full; the feeder will drain soon. Merge into the queued tail
		// by spinning briefly; under normal editing load this never fires.
		select {
		case s.changeCh <- c:
		case <-s.ctx.Done():
		}
	}
}

func (s *session) currentRange() Range {
	s.rangeMu.RLock()
	defer s.rangeMu.RUnlock()
	return s.rng
}

func (s *session) stopAndWait() {
	s.markStopping()
	s.cancel()
	// block until the feeder (which waits on prefetch) has fully cleaned up
	<-s.feederDone
}

// fail records the first terminal error and triggers shutdown.
func (s *session) fail(err error) {
	s.errOnce.Do(func() {
		if err != nil {
			s.err = err
			log.AU.Printf("playback aborting: %v", err)
		}
		s.markStopping()
		s.cancel()
	})
}

// Err returns the terminal error, if any, after the session has ended.
func (p *Player) Err() error {
	p.mu.Lock()
	sess := p.session
	p.mu.Unlock()
	if sess == nil {
		return nil
	}
	<-sess.feederDone
	return sess.err
}

// Peaks returns the max midi/wave amplitudes since the previous call and
// resets the running maxima. Safe to call from a GUI goroutine.
func (p *Player) Peaks() Peaks {
	p.mu.Lock()
	sess := p.session
	p.mu.Unlock()
	if sess == nil {
		return Peaks{}
	}
	sess.peakMu.Lock()
	out := Peaks{Midi: sess.midiPeak, Wave: sess.wavePeak}
	sess.midiPeak, sess.wavePeak = 0, 0
	sess.peakMu.Unlock()
	return out
}

func (s *session) run() error {
	if err := s.player.sink.Start(s.startPos); err != nil {
		s.fail(mkErr(ErrSink, err))
		// no workers were started; close both completion signals directly so
		// stopAndWait/Err never block on a session that failed at startup.
		close(s.prefetchDone)
		s.cleanup()
		return err
	}
	go s.prefetch()
	go s.feed()
	return nil
}
