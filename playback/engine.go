/*
Package playback contains the deterministic playback engine: it owns the
sample-prefetch goroutine and the synth/feeder goroutine, keeps loop range,
metronome beats and note events aligned with the audio output while any of
them change mid-playback, and guarantees a clean shutdown.

All I/O is expressed through small interfaces (SampleSource, AudioSink,
Synth, EventSource) so the engine can be exercised without audio hardware.
*/
package playback

import (
	"context"
	"errors"

	. "github.com/sqweek/sqribe/core/types"
)

// BlockFrames is the number of frames fed to the audio device per write.
const BlockFrames FrameN = 64

// PrefetchFrames is the size of each sample block fetched from the source.
// Must be a whole multiple of BlockFrames.
const PrefetchFrames FrameN = 2048

var (
	// ErrStopped is returned by engine methods after the session has been
	// stopped (or never started).
	ErrStopped = errors.New("playback: stopped")
	// ErrSource marks a failure reading sample data from the source; the
	// engine shuts down cleanly when this is reported.
	ErrSource = errors.New("playback: sample source read failed")
	// ErrSink marks a failure starting or writing the audio output.
	ErrSink = errors.New("playback: audio sink failure")
	// ErrEmptyRange is returned when asked to play an empty/invalid range.
	ErrEmptyRange = errors.New("playback: empty playback range")
)

// SampleSource provides the raw waveform samples the engine mixes over.
type SampleSource interface {
	// Frames returns the interleaved samples for the inclusive frame range.
	// A non-nil error must abort playback rather than block forever.
	Frames(ctx context.Context, f0, fN FrameN) ([]int16, error)
	// ToSample converts a frame count to an interleaved sample count.
	ToSample(frames FrameN) SampleN
	// ToFrame converts an interleaved sample count to a frame count.
	ToFrame(samples SampleN) FrameN
	// Channels returns the number of interleaved channels per frame.
	Channels() int
}

// AudioSink is the output device. Start begins playback at the given frame,
// Append blocks (applying back-pressure) while feeding one block, Rewind
// announces a loop wrap so the sink can reset its position bookkeeping, and
// Stop halts the device. Every call must be safe to make from the feeder
// goroutine; Start/Stop may additionally be called concurrently.
type AudioSink interface {
	Start(f0 FrameN) error
	Append(samples []int16) error
	Rewind(f0 FrameN)
	Stop()
	// PlayingFrame reports the frame currently at the DAC and whether the
	// sink is still running. ok==false means the device stalled.
	PlayingFrame() (frame FrameN, ok bool)
}

// Synth renders note/metronome events into a per-block int16 buffer.
type Synth interface {
	NoteOn(channel, note, velocity uint8)
	NoteOff(channel, note uint8)
	// Inst resolves the channel allocated for an instrument id.
	Inst(inst uint8) uint8
	// WriteFrames fills buf (interleaved stereo, BlockFrames frames).
	WriteFrames(buf []int16)
	// AllNotesOff releases every currently held note. Used on loop wraps and
	// on shutdown so no note survives across a boundary.
	AllNotesOff()
}

// NoteEvent is one score note clipped to the active loop range.
type NoteEvent struct {
	Start FrameN
	End   FrameN
	Pitch uint8
	// Inst selects the synth instrument/channel.
	Inst     uint8
	Velocity uint8
	Muted    bool
}

// MixLevels captures the gain/mute state applied while mixing a block.
type MixLevels struct {
	Master, Midi, Wave float64
	MuteMidi, MuteWave bool
	// MuteMetronome silences the beat clicks independently of note audio.
	MuteMetronome bool
}

// EventSource snapshots the score events and mixer state for a range.
// Implementations must be safe to call concurrently with score mutation;
// the engine only calls it from one goroutine at a time.
type EventSource interface {
	// Beats returns the metronome beat frames within [f0, fN].
	Beats(f0, fN FrameN) []FrameN
	// Notes returns the notes intersecting [f0, fN], sorted by Start.
	Notes(f0, fN FrameN) []NoteEvent
	// Levels returns a consistent snapshot of the mixer configuration.
	Levels() MixLevels
}

// Range is a loop interval with both ends inclusive.
type Range struct {
	Min, Max FrameN
}

func (r Range) valid() bool { return r.Min <= r.Max }

// Change flags score edits that require rebuilding the event lists.
type Change struct {
	Beat bool
	Note bool
}

func (c Change) empty() bool { return !c.Beat && !c.Note }

// Peaks holds the maximum absolute mixed sample amplitudes observed since the
// last read, used to drive UI level meters.
type Peaks struct {
	Midi float64
	Wave float64
}
