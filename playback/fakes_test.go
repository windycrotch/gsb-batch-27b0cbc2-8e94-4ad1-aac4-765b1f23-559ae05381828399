package playback

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"

	. "github.com/sqweek/sqribe/core/types"
)

// memSource serves a deterministic synthetic waveform in memory and can be
// configured to fail or block, exercising the engine's error handling.
type memSource struct {
	mu       sync.Mutex
	channels int
	data     []int16 // length = frames * channels
	failAt   *FrameN // when set, Frames touching >= failAt return err
	failErr  error
	block    chan struct{} // when non-nil, Frames block until closed or ctx ends
	reads    atomic.Int64
	// oob records any read whose frame range escapes [0, maxFrame]
	oob      []FrameRange
	maxFrame FrameN
}

func newMemSource(frames, channels int) *memSource {
	data := make([]int16, frames*channels)
	for f := 0; f < frames; f++ {
		for c := 0; c < channels; c++ {
			data[f*channels+c] = int16((f*7 + c*13) & 0x7fff)
		}
	}
	return &memSource{data: data, channels: channels, maxFrame: FrameN(frames) - 1}
}

func (m *memSource) Frames(ctx context.Context, f0, fN FrameN) ([]int16, error) {
	m.mu.Lock()
	block := m.block
	failAt := m.failAt
	failErr := m.failErr
	m.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f0 < 0 || fN > m.maxFrame || fN < f0 {
		m.mu.Lock()
		m.oob = append(m.oob, FrameRange{Min: f0, Max: fN})
		m.mu.Unlock()
		return nil, errors.New("out of bounds read")
	}
	if failAt != nil && fN >= *failAt {
		return nil, failErr
	}
	m.reads.Add(1)
	s0 := int(f0) * m.channels
	sN := (int(fN) + 1) * m.channels
	out := make([]int16, sN-s0)
	copy(out, m.data[s0:sN])
	return out, nil
}

func (m *memSource) ToSample(f FrameN) SampleN { return SampleN(f * FrameN(m.channels)) }
func (m *memSource) ToFrame(s SampleN) FrameN  { return FrameN(s / SampleN(m.channels)) }
func (m *memSource) Channels() int             { return m.channels }

func (m *memSource) setFail(f FrameN, err error) {
	m.mu.Lock()
	m.failAt = &f
	m.failErr = err
	m.mu.Unlock()
}

func (m *memSource) setBlock(ch chan struct{}) {
	m.mu.Lock()
	m.block = ch
	m.mu.Unlock()
}

func (m *memSource) oobReads() []FrameRange {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]FrameRange(nil), m.oob...)
}

// recSink records everything the engine pushes and never blocks on Append,
// while still tracking device lifecycle for assertions.
type recSink struct {
	mu        sync.Mutex
	started   atomic.Int32
	stopped   atomic.Int32
	rewinds   []FrameN
	appends   int
	samples   []int16
	startErr  error
	appendErr error
	failAfter int // return appendErr after this many appends
}

func (r *recSink) Start(f0 FrameN) error {
	if r.startErr != nil {
		return r.startErr
	}
	r.started.Add(1)
	r.mu.Lock()
	r.rewinds = append(r.rewinds, f0)
	r.mu.Unlock()
	return nil
}

func (r *recSink) Append(s []int16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appends++
	if r.appendErr != nil && r.appends > r.failAfter {
		return r.appendErr
	}
	r.samples = append(r.samples, s...)
	return nil
}

func (r *recSink) Rewind(f0 FrameN) {
	r.mu.Lock()
	r.rewinds = append(r.rewinds, f0)
	r.mu.Unlock()
}

func (r *recSink) Stop() { r.stopped.Add(1) }

func (r *recSink) PlayingFrame() (FrameN, bool) { return 0, true }

func (r *recSink) snapshot() (appends int, rewinds []FrameN, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appends, append([]FrameN(nil), r.rewinds...), len(r.samples)
}

// recSynth records note events and verifies AllNotesOff balance.
type recSynth struct {
	mu      sync.Mutex
	ons     []string
	offs    []string
	active  map[string]int
	instCh  map[uint8]uint8
	writes  int
	panicOn string // when set, NoteOn with this note panics to test robustness
}

func newRecSynth() *recSynth {
	return &recSynth{active: map[string]int{}, instCh: map[uint8]uint8{}}
}

func key(ch, n uint8) string {
	return string(rune(ch)) + ":" + string(rune(n))
}

func (s *recSynth) NoteOn(ch, note, vel uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ch, note)
	s.ons = append(s.ons, k)
	s.active[k]++
}

func (s *recSynth) NoteOff(ch, note uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ch, note)
	s.offs = append(s.offs, k)
	if s.active[k] > 0 {
		s.active[k]--
		if s.active[k] == 0 {
			delete(s.active, k)
		}
	}
}

func (s *recSynth) Inst(inst uint8) uint8 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.instCh[inst]; ok {
		return ch
	}
	ch := uint8(len(s.instCh))
	s.instCh[inst] = ch
	return ch
}

func (s *recSynth) WriteFrames(buf []int16) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	for i := range buf {
		buf[i] = 0
	}
}

func (s *recSynth) AllNotesOff() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.active {
		s.offs = append(s.offs, "all:"+k)
		delete(s.active, k)
	}
}

func (s *recSynth) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

func (s *recSynth) eventCounts() (ons, offs, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ons), len(s.offs), s.writes
}

// staticEvents serves fixed beat/note tables and a mutable mixer snapshot.
type staticEvents struct {
	mu     sync.Mutex
	beats  []FrameN
	notes  []NoteEvent
	levels MixLevels
}

func (e *staticEvents) Beats(f0, fN FrameN) []FrameN {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]FrameN, 0, len(e.beats))
	for _, b := range e.beats {
		if b >= f0 && b <= fN {
			out = append(out, b)
		}
	}
	return out
}

func (e *staticEvents) Notes(f0, fN FrameN) []NoteEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]NoteEvent, 0, len(e.notes))
	for _, n := range e.notes {
		if n.End > f0 && n.Start <= fN {
			c := n
			if c.Start < f0 {
				c.Start = f0
			}
			if c.End > fN {
				c.End = fN
			}
			out = append(out, c)
		}
	}
	return out
}

func (e *staticEvents) Levels() MixLevels {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.levels
}

func (e *staticEvents) setNotes(n []NoteEvent) {
	e.mu.Lock()
	e.notes = n
	e.mu.Unlock()
}

func (e *staticEvents) setBeats(b []FrameN) {
	e.mu.Lock()
	e.beats = b
	e.mu.Unlock()
}

var _ = bytes.MinRead
