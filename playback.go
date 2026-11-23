package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sqweek/sqribe/audio"
	"github.com/sqweek/sqribe/log"
	"github.com/sqweek/sqribe/midi"
	"github.com/sqweek/sqribe/playback"
	"github.com/sqweek/sqribe/score"
	"github.com/sqweek/sqribe/wave"

	. "github.com/sqweek/sqribe/core/types"
)

/* This file is the application glue around the deterministic playback engine
** in github.com/sqweek/sqribe/playback. All concurrency, loop wrapping, event
** alignment and shutdown logic lives in that package; here we only adapt the
** global G.* services to its interfaces and keep the GUI feedback loop. */

// player is the single engine instance, configured once an audio output exists.
var player *playback.Player

// playState mirrors the engine state for legacy call sites (UI key handling).
// Reads go through playStateVal() rather than touching a raw global.
func playStateVal() playback.State {
	if player == nil {
		return playback.StateStopped
	}
	return player.State()
}

// ---- SampleSource adapter ----

type waveSource struct{}

func (waveSource) Frames(ctx context.Context, f0, fN FrameN) ([]int16, error) {
	return G.wav.FramesCtx(ctx, f0, fN)
}

func (waveSource) ToSample(f FrameN) SampleN { return G.wav.ToSample(f) }
func (waveSource) ToFrame(s SampleN) FrameN  { return G.wav.ToFrame(s) }
func (waveSource) Channels() int             { return G.wav.Channels }

// ---- AudioSink adapter ----

type deviceSink struct{}

func (deviceSink) Start(f0 FrameN) error { return audio.Play(f0) }
func (deviceSink) Append(s []int16) error {
	audio.Append(s)
	return nil
}
func (deviceSink) Rewind(f0 FrameN) {
	// audio.Play on an already-running stream advances the position baseline
	if err := audio.Play(f0); err != nil {
		log.AU.Println("rewind failed:", err)
	}
}
func (deviceSink) Stop()                        { audio.Stop() }
func (deviceSink) PlayingFrame() (FrameN, bool) { return audio.PlayingFrame() }

// ---- Synth adapter ----

// synthAdapter wraps the global Synth and tracks held notes itself so
// AllNotesOff can release every note even though fluidsynth has no direct
// "all off" call. All access happens on the engine's feeder goroutine.
type synthAdapter struct {
	held map[noteKey]struct{}
}

type noteKey struct {
	ch, pitch uint8
}

func newSynthAdapter() *synthAdapter {
	return &synthAdapter{held: make(map[noteKey]struct{})}
}

func (a *synthAdapter) NoteOn(ch, pitch, vel uint8) {
	Synth.NoteOn(ch, pitch, vel)
	a.held[noteKey{ch, pitch}] = struct{}{}
}

func (a *synthAdapter) NoteOff(ch, pitch uint8) {
	Synth.NoteOff(ch, pitch)
	delete(a.held, noteKey{ch, pitch})
}

func (a *synthAdapter) Inst(inst uint8) uint8 { return Synth.Inst(inst) }

func (a *synthAdapter) WriteFrames(buf []int16) { Synth.WriteFrames(buf) }

func (a *synthAdapter) AllNotesOff() {
	for k := range a.held {
		Synth.NoteOff(k.ch, k.pitch)
		delete(a.held, k)
	}
}

// ---- EventSource adapter ----

// scoreEvents builds engine event snapshots under the score's serialised
// Snapshot lock and reads mixer state under mixerMu, so mid-playback edits
// never produce a torn view.
type scoreEvents struct {
	mixerMu *sync.RWMutex
}

func (e scoreEvents) Beats(f0, fN FrameN) []FrameN {
	var out []FrameN
	G.score.Snapshot(func(s *score.Score) {
		for _, f := range s.BeatFrames() {
			if f < f0 {
				continue
			}
			if f > fN {
				break
			}
			out = append(out, f)
		}
	})
	return out
}

func (e scoreEvents) Notes(f0, fN FrameN) []playback.NoteEvent {
	var out []playback.NoteEvent
	G.score.Snapshot(func(s *score.Score) {
		next := s.Iter(FrameRange{f0, fN})
		var sn score.StaffNote
		for next != nil {
			sn, next = next()
			start, _ := s.ToFrame(s.Beatf(sn.Note))
			end, _ := s.ToFrame(s.EndBeatf(sn.Note))
			if end <= f0 {
				continue
			}
			if start < f0 {
				start = f0
			}
			if end > fN {
				end = fN
			}
			mix := Mixer.For(sn.Staff)
			out = append(out, playback.NoteEvent{
				Start:    start,
				End:      end,
				Pitch:    sn.Note.Pitch,
				Inst:     uint8(mix.Voice),
				Velocity: uint8(mix.Velocity),
				Muted:    mix.Muted,
			})
		}
	})
	return out
}

func (e scoreEvents) Levels() playback.MixLevels {
	e.mixerMu.RLock()
	defer e.mixerMu.RUnlock()
	return playback.MixLevels{
		Master:        Mixer.Master.Gain,
		Midi:          Mixer.Midi.Gain,
		Wave:          Mixer.Wave.Gain,
		MuteMidi:      Mixer.Midi.Muted,
		MuteWave:      Mixer.Wave.Muted,
		MuteMetronome: Mixer.MuteMetronome,
	}
}

// mixerMu guards the Mixer fields read during playback against UI mutations.
var mixerMu sync.RWMutex

// withMixerLock runs a mixer mutation under the same lock the engine reads with.
func withMixerLock(fn func()) {
	mixerMu.Lock()
	defer mixerMu.Unlock()
	fn()
}

// PlayChange / coalescing retained for the score subscription plumbing.

type PlayChange struct {
	beat, note bool
}

func (pc *PlayChange) Empty() bool {
	return !pc.beat && !pc.note
}

func (pc *PlayChange) Update(event interface{}) {
	switch event.(type) {
	case score.BeatChanged:
		pc.beat = true
	case score.StaffChanged:
		pc.note = true
	}
}

func coalesced(out chan playback.Change) chan interface{} {
	in := make(chan interface{})
	go coalesce(in, out)
	return in
}

func coalesce(in chan interface{}, out chan playback.Change) {
	defer close(out)
	changes := PlayChange{}
	for in != nil {
		for changes.Empty() {
			if ev, open := <-in; open {
				changes.Update(ev)
			} else {
				return
			}
		}
		for !changes.Empty() {
			select {
			case ev2, open := <-in:
				if open {
					changes.Update(ev2)
				} else {
					in = nil
				}
			case out <- playback.Change{Beat: changes.beat, Note: changes.note}:
				changes = PlayChange{}
			}
		}
	}
}

// subKey uniquely identifies one session's score subscription so concurrent
// sessions cannot clobber each other's plumb entries.
type subKey struct{ id uint64 }

var subKeyCounter uint64

func newSubKey() *subKey {
	id := atomic.AddUint64(&subKeyCounter, 1)
	return &subKey{id: id}
}

func initPlayer() {
	if player != nil {
		return
	}
	player = playback.NewPlayer(waveSource{}, deviceSink{}, newSynthAdapter(), scoreEvents{mixerMu: &mixerMu})
}

func playToggle() {
	switch playStateVal() {
	case playback.StatePlaying:
		log.AU.Println("stopping playback")
		stopPlayback()
		return
	case playback.StateStopping:
		return /* in transition; do nothing */
	}

	if G.wav == nil {
		return
	}
	initPlayer()
	rng, loop := G.ww.SelectedTimeRange(), true
	if rng.MinFrame() >= rng.MaxFrame() {
		rng, loop = G.ww.WaveRange(), false
	}
	cursor := G.ww.FrameAtCursor()
	if cursor < rng.MinFrame() || cursor > rng.MaxFrame() {
		cursor = rng.MinFrame()
	}
	startPlayback(rng, cursor, loop)
}

func startPlayback(rng TimeRange, startPos FrameN, loop bool) {
	log.AU.Println("starting loop", rng.MinFrame(), rng.MaxFrame(), " @", startPos)
	// a unique key per session ensures a stale feedback loop can never
	// unsubscribe a newer session's score subscription.
	subKey := newSubKey()
	scorechan := make(chan playback.Change, 16)
	in := coalesced(scorechan)
	G.plumb.score.Sub(subKey, in)

	forwardDone := make(chan struct{})
	go func() {
		defer close(forwardDone)
		for c := range scorechan {
			if player != nil {
				player.NotifyChange(c)
			}
		}
	}()
	teardown := func() {
		G.plumb.score.Unsub(subKey)
		<-forwardDone
	}

	err := player.Start(playback.Range{Min: rng.MinFrame(), Max: rng.MaxFrame()}, startPos, loop)
	if err != nil {
		log.AU.Println("couldn't start playback:", err)
		teardown()
		return
	}
	go feedbackLoop(loop, teardown)
}

func stopPlayback() {
	// the feedback loop observes the device stop and performs the matching
	// subscription teardown; here we only halt the engine.
	player.Stop()
}

/* feedbackLoop drives the GUI cursor and level meters while a session runs. It
** exits as soon as the device reports it has stopped, and asks the engine to
** wind down if the audio callback was lost unexpectedly. */
func feedbackLoop(loop bool, teardown func()) {
	defer teardown()
	var mpeak, wpeak float64
	for {
		f, playing := audio.PlayingFrame()
		if !playing {
			if player != nil && player.State() == playback.StatePlaying && f != 0 {
				log.AU.Println("stopping: lost audio callback")
				player.Stop()
			}
			return
		}
		G.ww.SetCursorByFrame(f, !loop)
		peaks := player.Peaks()
		if peaks.Midi > mpeak {
			mpeak = peaks.Midi
		}
		if peaks.Wave > wpeak {
			wpeak = peaks.Wave
		}
		G.mixw.Levels(mpeak/32700, wpeak/32700)
		mpeak, wpeak = 0, 0
		time.Sleep(66 * time.Millisecond)
	}
}

// SetLoopRange applies a range adjustment (e.g. the user dragging the time
// axis) to the running engine. Returns false when nothing is playing.
func SetLoopRange(rng TimeRange) bool {
	if player == nil {
		return false
	}
	return player.SetRange(playback.Range{Min: rng.MinFrame(), Max: rng.MaxFrame()})
}

// silence unused import guards if midi is only referenced elsewhere
var _ = midi.InstWoodblock
var _ = wave.ErrCacheEOF
