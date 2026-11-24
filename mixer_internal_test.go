package audio

import (
	"context"
	"io"
	"testing"
	"time"

	"pipelined.dev/pipe"
	"pipelined.dev/pipe/mutable"
	"pipelined.dev/signal"
)

const (
	testBufferSize = 10
	testTimeout    = 5 * time.Second
)

var testProps = pipe.SignalProperties{
	Channels:   2,
	SampleRate: 44100,
}

func blocked() <-chan time.Time {
	return time.After(testTimeout)
}

func allocFrame(frames int) signal.Floating {
	return signal.Allocator{
		Channels: 2,
		Length:   frames,
		Capacity: testBufferSize,
	}.Float64()
}

// allocSink allocates a sink on m and fails the test on error.
func allocSink(t *testing.T, m *Mixer) pipe.Sink {
	t.Helper()
	s, err := m.Sink()(mutable.Mutable(), testBufferSize, testProps)
	if err != nil {
		t.Fatalf("sink allocation: %v", err)
	}
	return s
}

// TestMixerRejectMismatched verifies that inputs whose sample rate or
// channel count does not match the mixer are rejected before any buffer is
// allocated and never become part of the mixing set.
func TestMixerRejectMismatched(t *testing.T) {
	m := &Mixer{}
	allocSink(t, m)

	_, err := m.Sink()(mutable.Mutable(), testBufferSize, pipe.SignalProperties{
		Channels:   2,
		SampleRate: 48000,
	})
	if err != ErrDifferentSampleRates {
		t.Fatalf("expected ErrDifferentSampleRates, got %v", err)
	}
	_, err = m.Sink()(mutable.Mutable(), testBufferSize, pipe.SignalProperties{
		Channels:   1,
		SampleRate: 44100,
	})
	if err != ErrDifferentChannels {
		t.Fatalf("expected ErrDifferentChannels, got %v", err)
	}

	m.lock.Lock()
	got := len(m.inputs)
	m.lock.Unlock()
	if got != 1 {
		t.Fatalf("rejected inputs must not join the mixing set, got %d inputs", got)
	}
}

// TestMixerRejectAfterClose verifies that a sink allocated after the source
// terminated is rejected instead of leaking an input and its buffer.
func TestMixerRejectAfterClose(t *testing.T) {
	m := &Mixer{}
	allocSink(t, m)
	source, err := m.Source()(mutable.Mutable(), testBufferSize)
	if err != nil {
		t.Fatalf("source allocation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.StartFunc(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// simulate the single sink flushing and the source reaching EOF.
	m.inputs[0].signalDone()
	out := allocFrame(testBufferSize)
	if _, err := source.SourceFunc(out); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if err := source.FlushFunc(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	_, err = m.Sink()(mutable.Mutable(), testBufferSize, testProps)
	if err != errMixerClosed {
		t.Fatalf("expected errMixerClosed, got %v", err)
	}
	m.lock.Lock()
	got := len(m.inputs)
	closed := m.closed
	m.lock.Unlock()
	if got != 0 {
		t.Fatalf("inputs after teardown: got %d, want 0", got)
	}
	if !closed {
		t.Fatal("mixer must be marked closed")
	}
}

// TestMixerEarlyFinishRemovesInput drives the source directly: an input
// that flushes with a last buffered frame must be removed from the mixing
// set and its buffer returned to the pool on the source side, while the
// remaining input keeps mixing.
func TestMixerEarlyFinishRemovesInput(t *testing.T) {
	m := &Mixer{}
	firstSink := allocSink(t, m)
	shortSink := allocSink(t, m)
	source, err := m.Source()(mutable.Mutable(), testBufferSize)
	if err != nil {
		t.Fatalf("source allocation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.StartFunc(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := firstSink.StartFunc(ctx); err != nil {
		t.Fatalf("first sink start: %v", err)
	}
	if err := shortSink.StartFunc(ctx); err != nil {
		t.Fatalf("short sink start: %v", err)
	}

	m.lock.Lock()
	firstInput := m.inputs[0]
	shortInput := m.inputs[1]
	m.lock.Unlock()

	// both inputs produce a frame; the short one is partial and flushes.
	if err := firstSink.SinkFunc(allocFrame(testBufferSize)); err != nil {
		t.Fatalf("first sink: %v", err)
	}
	shortFrame := allocFrame(4)
	for i := 0; i < shortFrame.Len(); i++ {
		shortFrame.SetSample(i, 0.5)
	}
	if err := shortSink.SinkFunc(shortFrame); err != nil {
		t.Fatalf("short sink: %v", err)
	}
	if err := shortSink.FlushFunc(ctx); err != nil {
		t.Fatalf("short flush: %v", err)
	}

	out := allocFrame(testBufferSize)
	n, err := source.SourceFunc(out)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	// the mixed frame spans the longest input (10 frames); the short input
	// contributes its 0.5 frame to the first 4 frames (8 interleaved
	// samples), the first input's frame is zero-filled.
	if n != testBufferSize {
		t.Fatalf("mixed samples: got %d, want %d (length of the longest frame)", n, testBufferSize)
	}
	for i := 0; i < 4*2; i++ {
		if got := out.Sample(i); got != 0.25 {
			t.Fatalf("mixed sample %d: got %v, want 0.25 (average of zero and 0.5)", i, got)
		}
	}
	for i := 4 * 2; i < n*2; i++ {
		if got := out.Sample(i); got != 0 {
			t.Fatalf("sample %d beyond the short frame: got %v, want 0", i, got)
		}
	}

	// remaining input flushes as well -> source reaches EOF and tears down.
	if err := firstSink.FlushFunc(ctx); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if _, err := source.SourceFunc(out); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if err := source.FlushFunc(ctx); err != nil {
		t.Fatalf("source flush: %v", err)
	}
	// after the full lifecycle every input must have been removed and its
	// buffer returned to the pool exactly once.
	if !shortInput.freed {
		t.Fatal("finished input buffer must be returned to the pool on the source side")
	}
	if !firstInput.freed {
		t.Fatal("remaining input buffer must be returned on teardown")
	}
	m.lock.Lock()
	got := len(m.inputs)
	closed := m.closed
	m.lock.Unlock()
	if got != 0 {
		t.Fatalf("inputs after teardown: got %d, want 0", got)
	}
	if !closed {
		t.Fatal("mixer must be marked closed")
	}
}

// TestMixerCancelReclaimsInputs cancels the context while sinks may be
// parked waiting for the source. Teardown must unblock every goroutine and
// return all input buffers and the output buffer to their pools.
func TestMixerCancelReclaimsInputs(t *testing.T) {
	m := &Mixer{}
	sinks := make([]pipe.Sink, 0, 3)
	for i := 0; i < 3; i++ {
		s := allocSink(t, m)
		sinks = append(sinks, s)
	}
	source, err := m.Source()(mutable.Mutable(), testBufferSize)
	if err != nil {
		t.Fatalf("source allocation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := source.StartFunc(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, s := range sinks {
		if err := s.StartFunc(ctx); err != nil {
			t.Fatalf("sink start: %v", err)
		}
	}

	m.lock.Lock()
	inputs := append([]*mixerInput(nil), m.inputs...)
	m.lock.Unlock()

	cancel()

	// On cancel the sink goroutines exit their SinkFunc context branches
	// and reach the flush hooks.
	done := make(chan struct{})
	go func() {
		for _, s := range sinks {
			if err := s.FlushFunc(ctx); err != nil {
				t.Errorf("sink flush: %v", err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-blocked():
		t.Fatal("sink flush hooks must not block after cancellation")
	}

	// Source observes cancellation and the flush hook reclaims everything.
	out := allocFrame(testBufferSize)
	if _, err := source.SourceFunc(out); err != io.EOF {
		t.Fatalf("expected io.EOF on cancel, got %v", err)
	}
	flushDone := make(chan struct{})
	go func() {
		if err := source.FlushFunc(ctx); err != nil {
			t.Errorf("source flush: %v", err)
		}
		close(flushDone)
	}()
	select {
	case <-flushDone:
	case <-blocked():
		t.Fatal("source flush must converge after cancellation")
	}

	m.lock.Lock()
	remaining := len(m.inputs)
	closed := m.closed
	m.lock.Unlock()
	if remaining != 0 {
		t.Fatalf("inputs after teardown: got %d, want 0", remaining)
	}
	if !closed {
		t.Fatal("mixer must be marked closed after teardown")
	}
	for i, in := range inputs {
		if !in.freed {
			t.Fatalf("input %d buffer not returned to the pool", i)
		}
	}
}
