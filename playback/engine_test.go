package playback

import (
	"errors"
	"sync"
	"testing"
	"time"

	. "github.com/sqweek/sqribe/core/types"
)

const testChannels = 2

func newHarness(t *testing.T, frames int) (*Player, *memSource, *recSink, *recSynth, *staticEvents) {
	t.Helper()
	src := newMemSource(frames, testChannels)
	sink := &recSink{}
	synth := newRecSynth()
	events := &staticEvents{
		beats:  []FrameN{0, 64, 128, 192, 256, 320, 384, 448, 512},
		levels: MixLevels{Master: 1, Midi: 1, Wave: 1},
	}
	p := NewPlayer(src, sink, synth, events)
	return p, src, sink, synth, events
}

// waitFor blocks until cond is true or the test deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestNormalLoop verifies that a looping session rewinds at the range end and
// replays beat/note events from the top without leaving notes ringing.
func TestNormalLoop(t *testing.T) {
	frames := 2048
	p, src, sink, synth, events := newHarness(t, frames)
	events.setNotes([]NoteEvent{
		{Start: 10, End: 40, Pitch: 60, Inst: 1, Velocity: 100},
		{Start: 130, End: 170, Pitch: 64, Inst: 1, Velocity: 100},
	})
	rng := Range{Min: 0, Max: 255}

	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	// expect at least two wraps (rewinds beyond the initial Start baseline)
	waitFor(t, "second loop wrap", func() bool {
		_, rewinds, _ := sink.snapshot()
		return len(rewinds) >= 3
	})
	if oob := src.oobReads(); len(oob) != 0 {
		t.Fatalf("out of bounds reads during loop: %v", oob)
	}
	p.Stop()
	if got := p.State(); got != StateStopped {
		t.Fatalf("state after stop = %v, want stopped", got)
	}
	if sink.stopped.Load() == 0 {
		t.Fatal("sink.Stop was not called")
	}
	if n := synth.activeCount(); n != 0 {
		t.Fatalf("%d notes still active after stop", n)
	}
	appends, _, nsamp := sink.snapshot()
	if appends == 0 || nsamp == 0 {
		t.Fatal("no audio was fed to the sink")
	}
	if nsamp%(int(BlockFrames)*testChannels) != 0 {
		t.Fatalf("fed %d samples, not a multiple of block size %d", nsamp, BlockFrames)
	}
}

// TestRangeChangeDuringPlayback changes the loop range mid-session and asserts
// the new bounds take effect without any out-of-bounds source reads.
func TestRangeChangeDuringPlayback(t *testing.T) {
	frames := 4096
	p, src, sink, _, events := newHarness(t, frames)
	events.setBeats([]FrameN{0, 64, 128, 192, 256})
	rng := Range{Min: 0, Max: 1023}
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	// let it play, then shrink and shift the range while running
	time.Sleep(30 * time.Millisecond)
	newRng := Range{Min: 512, Max: 767}
	if !p.SetRange(newRng) {
		t.Fatal("SetRange returned false while playing")
	}
	waitFor(t, "wrap within new range", func() bool {
		_, rewinds, _ := sink.snapshot()
		return len(rewinds) >= 2
	})
	// and grow it again
	newRng2 := Range{Min: 256, Max: 1535}
	if !p.SetRange(newRng2) {
		t.Fatal("second SetRange returned false")
	}
	time.Sleep(30 * time.Millisecond)
	p.Stop()
	for _, r := range src.oobReads() {
		if r.Min < 0 || r.Max > FrameN(frames-1) {
			t.Fatalf("out of bounds read after range change: %v", r)
		}
	}
	appends, _, _ := sink.snapshot()
	if appends == 0 {
		t.Fatal("no audio fed after range changes")
	}
}

// TestStopAndReplay starts, stops, then starts again and confirms the second
// session runs cleanly with balanced lifecycle calls.
func TestStopAndReplay(t *testing.T) {
	frames := 1024
	p, _, sink, synth, events := newHarness(t, frames)
	events.setNotes([]NoteEvent{{Start: 5, End: 30, Pitch: 48, Inst: 0, Velocity: 90}})
	rng := Range{Min: 0, Max: 511}

	for i := 0; i < 3; i++ {
		if err := p.Start(rng, 0, true); err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
		waitFor(t, "audio output", func() bool {
			appends, _, _ := sink.snapshot()
			return appends > 0
		})
		p.Stop()
		if p.State() != StateStopped {
			t.Fatalf("iteration %d: not stopped", i)
		}
		if n := synth.activeCount(); n != 0 {
			t.Fatalf("iteration %d: %d active notes", i, n)
		}
	}
	if sink.started.Load() != 3 {
		t.Fatalf("sink started %d times, want 3", sink.started.Load())
	}
	if sink.stopped.Load() != 3 {
		t.Fatalf("sink stopped %d times, want 3", sink.stopped.Load())
	}
}

// TestSourceReadFailure makes the cache source fail mid-playback and checks the
// engine shuts down to a restartable state instead of hanging or panicking.
func TestSourceReadFailure(t *testing.T) {
	frames := 4096
	p, src, sink, synth, _ := newHarness(t, frames)
	boom := errors.New("chunk not written")
	src.setFail(900, boom)
	rng := Range{Min: 0, Max: 2047}
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "engine to stop after source failure", func() bool {
		return p.State() == StateStopped
	})
	if sink.stopped.Load() == 0 {
		t.Fatal("sink not stopped after source failure")
	}
	if n := synth.activeCount(); n != 0 {
		t.Fatalf("%d notes left ringing after failure", n)
	}
	// the player must be restartable once the source recovers
	src.setFail(FrameN(frames+1), nil)
	if err := p.Start(Range{Min: 0, Max: 255}, 0, false); err != nil {
		t.Fatalf("restart after failure: %v", err)
	}
	waitFor(t, "post-failure audio", func() bool {
		appends, _, _ := sink.snapshot()
		return appends > 0
	})
	p.Stop()
}

// TestBlockedSourceUnblocksOnStop covers the "block not written yet" case: the
// prefetch must wait interruptibly and terminate promptly when Stop is called,
// never waiting forever.
func TestBlockedSourceUnblocksOnStop(t *testing.T) {
	frames := 4096
	p, src, sink, _, _ := newHarness(t, frames)
	gate := make(chan struct{})
	src.setBlock(gate)
	rng := Range{Min: 0, Max: 2047}
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return while source read was blocked")
	}
	if sink.stopped.Load() == 0 {
		t.Fatal("sink not stopped")
	}
}

// TestPeaksReportedAndReset confirms the meter peak capture reflects audible
// waveform/midi content and is reset on read.
func TestPeaksReportedAndReset(t *testing.T) {
	frames := 1024
	p, _, sink, _, events := newHarness(t, frames)
	// memSource produces non-zero wave samples, so the wave peak must be > 0
	rng := Range{Min: 0, Max: 511}
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "output", func() bool {
		appends, _, _ := sink.snapshot()
		return appends > 10
	})
	waitFor(t, "wave peak", func() bool {
		return p.Peaks().Wave > 0
	})
	p.Stop()
	// first read drains whatever accumulated up to shutdown; the second read
	// must return zeros, proving the accumulator resets.
	_ = p.Peaks()
	if got := p.Peaks(); got.Wave != 0 || got.Midi != 0 {
		t.Fatalf("peaks not reset on read: %+v", got)
	}
	_ = events
}

// TestSinkStartFailure makes the device reject Start and checks the player
// reports the error and remains restartable.
func TestSinkStartFailure(t *testing.T) {
	frames := 1024
	p, _, sink, _, _ := newHarness(t, frames)
	sink.startErr = errors.New("device unavailable")
	rng := Range{Min: 0, Max: 511}
	if err := p.Start(rng, 0, true); err == nil {
		t.Fatal("expected Start to surface sink error")
	}
	waitFor(t, "stopped state after start failure", func() bool {
		return p.State() == StateStopped
	})
	// recover and play successfully
	sink.startErr = nil
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("restart after sink failure: %v", err)
	}
	waitFor(t, "audio after recovery", func() bool {
		appends, _, _ := sink.snapshot()
		return appends > 0
	})
	p.Stop()
}

// TestConcurrentStartStop hammers the engine with concurrent lifecycle calls,
// range edits and score changes to surface races/deadlocks (run with -race).
func TestConcurrentStartStop(t *testing.T) {
	frames := 8192
	p, _, sink, _, events := newHarness(t, frames)
	events.setNotes([]NoteEvent{{Start: 20, End: 80, Pitch: 55, Inst: 2, Velocity: 110}})
	rng := Range{Min: 0, Max: 4095}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 25; k++ {
				switch k % 4 {
				case 0:
					_ = p.Start(rng, FrameN((i*k)*64%4000), true)
				case 1:
					p.Stop()
				case 2:
					p.SetRange(Range{Min: FrameN(k * 32 % 2000), Max: FrameN(2048 + k*32)})
				case 3:
					p.NotifyChange(Change{Beat: k%2 == 0, Note: true})
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	p.Stop()
	waitFor(t, "quiescence", func() bool { return p.State() == StateStopped })
	if sink.stopped.Load() == 0 {
		t.Fatal("sink never stopped under concurrent load")
	}
}

// TestEventsRestartOnWrap specifically asserts note and metronome events are
// re-triggered on each wrap and every note is released across the boundary.
func TestEventsRestartOnWrap(t *testing.T) {
	frames := 2048
	p, _, sink, synth, events := newHarness(t, frames)
	events.setBeats([]FrameN{0, 128})
	events.setNotes([]NoteEvent{{Start: 10, End: 60, Pitch: 60, Inst: 1, Velocity: 100}})
	rng := Range{Min: 0, Max: 255}
	if err := p.Start(rng, 0, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "multiple wraps", func() bool {
		_, rewinds, _ := sink.snapshot()
		return len(rewinds) >= 4
	})
	p.Stop()
	ons, offs, _ := synth.eventCounts()
	// each wrap replays the note and two beats; ons must be matched by offs and
	// nothing may remain active
	if ons < 4 {
		t.Fatalf("expected repeated note-ons across wraps, got %d", ons)
	}
	if offs < ons {
		t.Fatalf("note-offs %d fewer than note-ons %d", offs, ons)
	}
	if n := synth.activeCount(); n != 0 {
		t.Fatalf("%d notes leaked across wrap/stop", n)
	}
}
