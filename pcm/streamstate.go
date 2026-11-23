// SPDX-License-Identifier: LGPL-2.1-or-later

package pcm

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrConcurrentUse reports that one Encoder or FrameEncoder was entered
// concurrently from two goroutines. Both types are single-stream, stateful
// objects and are documented as not safe for concurrent use; the correct
// shape is one encoder per goroutine. Rather than letting the two calls
// interleave codec state and emit corrupted, intermingled frames, every
// mutating entry point takes a non-blocking lock and the losing call gets
// this sentinel while the winning call runs undisturbed. The error does not
// end the stream: the encoder stays in exactly the state the winning call
// leaves it in, and the loser is expected to stop sharing the encoder, not
// to Reset it.
var ErrConcurrentUse = errors.New("go-aac/pcm: concurrent use of one encoder; use one encoder per goroutine")

// EncodeStage names the phase of the encode pipeline a stream last reached.
// It is the machine-readable half of the failure boundary a caller needs
// when a sink or an emit callback rejects a frame: together with
// StreamState.Frames it says exactly how much of the output is complete and
// therefore reusable.
type EncodeStage int

const (
	// StageNone is the state of an encoder that has never been configured,
	// has just been through a successful Reset (no audio processed yet), or
	// is left unconfigured by a failed Reset.
	StageNone EncodeStage = iota
	// StageWrite is a failure returned from Write / EncodeInterleaved while
	// PCM was being consumed: the codec advanced but the output of one frame
	// was rejected, so the stream is broken at a frame boundary.
	StageWrite
	// StageFlush is a failure returned from Close / Flush while the trailing
	// partial frame was being encoded or the one-frame encoder delay was
	// being drained. Every frame the caller saw before it is complete; only
	// the final, never-delivered frame is missing.
	StageFlush
	// StageDone is a stream closed without an error: the drain finished and
	// every access unit was delivered. Reset moves the encoder back to
	// StageNone for the next stream.
	StageDone
)

// String gives the stage a short, stable label for logs and error reports.
func (s EncodeStage) String() string {
	switch s {
	case StageNone:
		return "none"
	case StageWrite:
		return "write"
	case StageFlush:
		return "flush"
	case StageDone:
		return "done"
	default:
		return "unknown"
	}
}

// StreamState is the recoverable boundary record of one encoder's current
// stream, returned by Encoder.StreamState and FrameEncoder.StreamState. A
// batch caller that pools encoders reads it after an error to decide what to
// discard: the failed stream's sink already holds Frames complete ADTS frames
// (or access units), and no partial frame can ever be among them because an
// access unit is handed to the sink or callback whole, in one call.
//
// All fields are read with atomics, so StreamState itself is safe to call
// concurrently with an in-flight encode even though the encoder's other
// methods are not; Err is only meaningful once the failing call has returned.
type StreamState struct {
	// Stage is the pipeline phase last reached. After a successful Reset it
	// is StageNone; a clean Close/Flush leaves StageDone; any failure leaves
	// StageWrite or StageFlush naming where it happened. A failed Reset
	// leaves StageNone together with a zero Frames.
	Stage EncodeStage
	// Frames is the number of complete frames durably delivered to the sink
	// (Encoder: whole ADTS frames written; FrameEncoder: access units passed
	// to emit) since the last successful Reset. The priming frame produces
	// no output and is not counted. A frame whose sink write or emit call
	// failed is not counted either, so Frames always describes output the
	// caller actually received.
	Frames int
	// Err is the terminal error that ended the stream, or nil while the
	// stream is live and after a clean Close/Flush. A successful Reset
	// clears it, so an error from one stream can never surface on the next.
	Err error
}

// streamStatus is the internal, concurrency-safe state holder shared by
// FrameEncoder and the Encoder wrapper. The counters and stage are atomics
// so StreamState can snapshot them without taking the call lock; the error
// is an atomic pointer so the snapshot read can never race the failing
// call's write, with nil the natural "no error" value (atomic.Value cannot
// store a typed nil, so it would not work here). The lock itself is a
// TryLock guard: it is never held across a sink or emit callback's return,
// which keeps a callback that drives the same encoder from deadlocking (it
// gets ErrConcurrentUse instead, the documented misuse result).
type streamStatus struct {
	lock   sync.Mutex
	stage  atomic.Int32
	frames atomic.Int64
	err    atomic.Pointer[error]
}

// snapshot reads the boundary record atomically. A nil pointer reads back
// as a nil error without an allocation on the common live-stream path.
func (s *streamStatus) snapshot() StreamState {
	var err error
	if p := s.err.Load(); p != nil {
		err = *p
	}
	return StreamState{
		Stage:  EncodeStage(s.stage.Load()),
		Frames: int(s.frames.Load()),
		Err:    err,
	}
}

// begin guards one mutating encoder entry point against a concurrent caller.
// It returns false (and the caller returns ErrConcurrentUse without touching
// any stream state) when another call is in flight; otherwise the caller owns
// the encoder until end runs.
func (s *streamStatus) begin() bool {
	return s.lock.TryLock()
}

// end releases the guard taken by begin.
func (s *streamStatus) end() {
	s.lock.Unlock()
}

// resetFor clears the per-stream boundary record for a new stream. It runs
// while the caller holds the lock (or, from a disarm path, with no other
// goroutine able to enter), and the atomic stores also publish the cleared
// state to a concurrent StreamState reader.
func (s *streamStatus) resetFor() {
	s.stage.Store(int32(StageNone))
	s.frames.Store(0)
	s.err.Store(nil)
}

// frameDelivered accounts for one whole frame the sink or callback accepted.
func (s *streamStatus) frameDelivered() {
	s.frames.Add(1)
}

// fail records the terminal error and the phase it happened in, keeping the
// first failure if several paths race to record one (they cannot in
// practice, the call lock serializes entries, but the guard keeps the record
// monotonic like the latched error it mirrors).
func (s *streamStatus) fail(stage EncodeStage, err error) {
	if err == nil {
		return
	}
	if s.err.CompareAndSwap(nil, &err) {
		s.stage.Store(int32(stage))
	}
}

// done marks a stream drained without error.
func (s *streamStatus) done() {
	if s.err.Load() == nil {
		s.stage.Store(int32(StageDone))
	}
}
