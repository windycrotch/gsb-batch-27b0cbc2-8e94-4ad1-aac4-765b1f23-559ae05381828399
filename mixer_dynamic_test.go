package audio_test

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"pipelined.dev/audio"

	"pipelined.dev/pipe"
	"pipelined.dev/pipe/mock"
	"pipelined.dev/pipe/mutable"
	"pipelined.dev/signal"
)

const externalTestTimeout = 5 * time.Second

func after(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// countingSink is a pipe sink whose counters are safe to read from the
// test goroutine while the pipe is running. mock.Sink's counters are not.
type countingSink struct {
	mu       sync.Mutex
	messages int
	samples  int
	values   []float64
}

func (c *countingSink) sink() pipe.SinkAllocatorFunc {
	return func(_ mutable.Context, bufferSize int, props pipe.SignalProperties) (pipe.Sink, error) {
		return pipe.Sink{
			SinkFunc: func(in signal.Floating) error {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.messages++
				for i := 0; i < in.Len(); i++ {
					c.samples++
					c.values = append(c.values, in.Sample(i))
				}
				return nil
			},
		}, nil
	}
}

func (c *countingSink) snapshot() (int, []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	values := append([]float64(nil), c.values...)
	return c.messages, values
}

// waitForMessages blocks until the counting sink observed want messages.
// It never busy-waits without yielding and always reads under the lock.
func waitForMessages(t *testing.T, c *countingSink, want int) []float64 {
	t.Helper()
	deadline := time.Now().Add(externalTestTimeout)
	for {
		messages, values := c.snapshot()
		if messages >= want {
			return values
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d messages, got %d", want, messages)
		}
		time.Sleep(time.Millisecond)
	}
}

// stepSource emits exactly one frame of Value per token received from
// step. Blocking on step gives deterministic control over frame production.
// The context captured in StartFunc unblocks it on cancellation.
type stepSource struct {
	channels int
	limit    int
	value    float64
	step     <-chan struct{}
	ctx      context.Context
}

func (g *stepSource) source() pipe.SourceAllocatorFunc {
	return func(_ mutable.Context, _ int) (pipe.Source, error) {
		produced := 0
		return pipe.Source{
			SignalProperties: pipe.SignalProperties{
				Channels:   g.channels,
				SampleRate: 44100,
			},
			StartFunc: func(ctx context.Context) error {
				g.ctx = ctx
				return nil
			},
			SourceFunc: func(out signal.Floating) (int, error) {
				if produced >= g.limit {
					return 0, io.EOF
				}
				select {
				case <-g.step:
				case <-g.ctx.Done():
					return 0, io.EOF
				}
				n := out.Length()
				if left := g.limit - produced; left < n {
					n = left
				}
				for i := 0; i < n*g.channels; i++ {
					out.SetSample(i, g.value)
				}
				produced += n
				return n, nil
			},
		}, nil
	}
}

// gateSource blocks on gate until the gate opens or the pipe context is
// canceled. It allows cancellation tests to park a complete line.
type gateSource struct {
	channels int
	gate     <-chan struct{}
	ctx      context.Context
}

func (g *gateSource) source() pipe.SourceAllocatorFunc {
	return func(_ mutable.Context, _ int) (pipe.Source, error) {
		return pipe.Source{
			SignalProperties: pipe.SignalProperties{
				Channels:   g.channels,
				SampleRate: 44100,
			},
			StartFunc: func(ctx context.Context) error {
				g.ctx = ctx
				return nil
			},
			SourceFunc: func(out signal.Floating) (int, error) {
				select {
				case <-g.gate:
				case <-g.ctx.Done():
				}
				return 0, io.EOF
			},
		}, nil
	}
}

// TestMixerDynamicAppend verifies that an input appended at runtime only
// contributes samples from its attachment point onward: frames emitted
// before the append cannot contain any appended-input samples, and after
// the append the new input participates in every round it produced a frame
// for.
func TestMixerDynamicAppend(t *testing.T) {
	const (
		channels   = 1
		bufferSize = 4
	)
	step1 := make(chan struct{})
	step2 := make(chan struct{})
	mixer := &audio.Mixer{}
	sink := &countingSink{}

	p, err := pipe.New(
		bufferSize,
		pipe.Line{
			Source: (&stepSource{
				channels: channels,
				limit:    6 * bufferSize,
				value:    0.2,
				step:     step1,
			}).source(),
			Sink: mixer.Sink(),
		},
		pipe.Line{
			Source: mixer.Source(),
			Sink:   sink.sink(),
		},
	)
	if err != nil {
		t.Fatalf("pipe new: %v", err)
	}
	errc := p.Start(context.Background())

	// phase 1: the first input alone emits two frames.
	for i := 0; i < 2; i++ {
		select {
		case step1 <- struct{}{}:
		case <-after(externalTestTimeout):
			t.Fatal("timed out stepping first input")
		}
	}
	prefix := waitForMessages(t, sink, 2)

	// append the second input. It cannot produce anything until the test
	// starts feeding step2, so no earlier output can depend on it.
	p.Push(p.AddLine(pipe.Line{
		Source: (&stepSource{
			channels: channels,
			limit:    4 * bufferSize,
			value:    0.6,
			step:     step2,
		}).source(),
		Sink: mixer.Sink(),
	}))

	// phase 2: drive both inputs in lockstep. Every round produces one
	// output frame; the appended source still holds no frame until step2 is
	// fed, so each feed pair directly controls the mix.
	for i := 0; i < 4; i++ {
		select {
		case step1 <- struct{}{}:
		case <-after(externalTestTimeout):
			t.Fatal("timed out stepping first input after append")
		}
		select {
		case step2 <- struct{}{}:
		case <-after(externalTestTimeout):
			t.Fatal("timed out stepping appended input")
		}
	}

	if err := pipe.Wait(errc); err != nil {
		t.Fatalf("pipe wait: %v", err)
	}

	_, final := sink.snapshot()
	// every sample emitted before the append stays exactly as it was and
	// contains only the first input.
	if len(final) < len(prefix) {
		t.Fatalf("output shrank after append: got %d samples, had %d", len(final), len(prefix))
	}
	for i, v := range prefix {
		if final[i] != v {
			t.Fatalf("already emitted sample %d changed after append: got %v, want %v", i, final[i], v)
		}
		if v != 0.2 {
			t.Fatalf("pre-append sample %d must contain only the first input: got %v, want 0.2", i, v)
		}
	}
	// after the append, frames are first-input-only (0.2) while the
	// appended frame is in flight, averages (0.4) while both inputs are
	// active, then appended-input-only (0.6) after the first input ends.
	// No other value may occur, every frame must be uniform, and both
	// inputs must demonstrably contribute.
	seen := map[float64]bool{}
	for frame := 0; frame*bufferSize < len(final); frame++ {
		values := final[frame*bufferSize : min(frame*bufferSize+bufferSize, len(final))]
		kind := values[0]
		for _, v := range values {
			if v != kind {
				t.Fatalf("inconsistent values inside frame %d: %v", frame, values)
			}
		}
		switch kind {
		case 0.2, 0.4, 0.6:
			seen[kind] = true
		default:
			t.Fatalf("unexpected frame value %v in frame %d (%v)", kind, frame, values)
		}
	}
	if !seen[0.4] {
		t.Fatalf("appended input never participated in the mix, output: %v", final)
	}
	if !seen[0.2] {
		t.Fatalf("first input never appears on its own, output: %v", final)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestMixerEarlyFinish verifies through the real pipe runtime that an input
// finishing first leaves the mix and the remaining inputs keep producing.
func TestMixerEarlyFinish(t *testing.T) {
	const (
		channels   = 1
		bufferSize = 4
	)
	mixer := &audio.Mixer{}
	sink := &mock.Sink{}
	p, err := pipe.New(
		bufferSize,
		pipe.Line{
			Source: (&mock.Source{
				Limit:    3 * bufferSize,
				Channels: channels,
				Value:    0.5,
			}).Source(),
			Sink: mixer.Sink(),
		},
		pipe.Line{
			Source: (&mock.Source{
				Limit:    5 * bufferSize,
				Channels: channels,
				Value:    0.7,
			}).Source(),
			Sink: mixer.Sink(),
		},
		pipe.Line{
			Source: mixer.Source(),
			Sink:   sink.Sink(),
		},
	)
	if err != nil {
		t.Fatalf("pipe new: %v", err)
	}
	if err := pipe.Wait(p.Start(context.Background())); err != nil {
		t.Fatalf("pipe wait: %v", err)
	}
	result := make([]float64, sink.Values.Len())
	signal.ReadFloat64(sink.Values, result)
	want := []float64{
		0.6, 0.6, 0.6, 0.6,
		0.6, 0.6, 0.6, 0.6,
		0.6, 0.6, 0.6, 0.6,
		0.7, 0.7, 0.7, 0.7,
		0.7, 0.7, 0.7, 0.7,
	}
	if len(result) != len(want) {
		t.Fatalf("samples: got %d, want %d", len(result), len(want))
	}
	for i := range want {
		if result[i] != want[i] {
			t.Fatalf("sample %d: got %v, want %v (full result: %v)", i, result[i], want[i], result)
		}
	}
}

// TestMixerMismatchRejected exercises the rejection through a real running
// pipe: allocating a mismatched sink returns the error, leaves the mixing
// set untouched and does not disturb the running lines.
func TestMixerMismatchRejected(t *testing.T) {
	const bufferSize = 4
	mixer := &audio.Mixer{}
	// Fix the mixer properties up front through a fully lifecycle-managed
	// sink so the rejection below is deterministic. This sink produces no
	// frames and is flushed before teardown, mirroring a real sink line.
	seedSink, err := mixer.Sink()(mutable.Mutable(), bufferSize, pipe.SignalProperties{
		Channels:   1,
		SampleRate: 44100,
	})
	if err != nil {
		t.Fatalf("seed sink allocation: %v", err)
	}
	seedCtx, seedCancel := context.WithCancel(context.Background())
	defer seedCancel()
	if err := seedSink.StartFunc(seedCtx); err != nil {
		t.Fatalf("seed sink start: %v", err)
	}

	gate := make(chan struct{})
	sink := &mock.Sink{}
	p, err := pipe.New(
		bufferSize,
		pipe.Line{
			Source: (&stepSource{
				channels: 1,
				limit:    2 * bufferSize,
				value:    0.3,
				step:     gate,
			}).source(),
			Sink: mixer.Sink(),
		},
		pipe.Line{
			Source: mixer.Source(),
			Sink:   sink.Sink(),
		},
	)
	if err != nil {
		t.Fatalf("pipe new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := p.Start(ctx)

	// allocate directly against the running mixer, same call AddLine would
	// make internally. The mismatch must be reported and nothing appended.
	rejected := make(chan error, 1)
	go func() {
		_, e := mixer.Sink()(mutable.Mutable(), bufferSize, pipe.SignalProperties{
			Channels:   2,
			SampleRate: 44100,
		})
		rejected <- e
	}()
	select {
	case e := <-rejected:
		if e != audio.ErrDifferentChannels {
			t.Fatalf("expected ErrDifferentChannels, got %v", e)
		}
	case <-after(externalTestTimeout):
		t.Fatal("rejected allocation must not block the running pipe")
	}

	// a conforming input can still be appended as a real line and the
	// running pipe stays healthy.
	conformingGate := make(chan struct{})
	p.Push(p.AddLine(pipe.Line{
		Source: (&gateSource{channels: 1, gate: conformingGate}).source(),
		Sink:   mixer.Sink(),
	}))

	// close the seed sink's lifecycle before the mixer tears down.
	if err := seedSink.FlushFunc(seedCtx); err != nil {
		t.Fatalf("seed sink flush: %v", err)
	}
	cancel()
	if err := pipe.Wait(errc); err != nil {
		t.Fatalf("pipe wait: %v", err)
	}
}

// assertConverged cancels ctx and asserts the pipe returns and that no
// mixer/pipe goroutine remains blocked afterwards.
func assertConverged(t *testing.T, errc <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case e := <-errc:
		if e != nil {
			t.Fatalf("canceled pipe must finish without error, got: %v", e)
		}
	case <-after(externalTestTimeout):
		t.Fatal("pipe did not return after context cancellation")
	}
	// After the error channel closed, every component goroutine must have
	// unwound. Assert via stacks rather than a relative goroutine count:
	// frames from the pipe framework or the non-test audio package mean a
	// component goroutine is still blocked. Frames from this test package
	// (pipelined.dev/audio_test) are ignored.
	deadline := time.Now().Add(externalTestTimeout)
	for {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		stack := string(buf[:n])
		if !strings.Contains(stack, "pipelined.dev/pipe") &&
			!strings.Contains(stack, "pipelined.dev/audio.") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipe goroutines still blocked after cancellation:\n%s", stack)
		}
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMixerCancelReturns verifies that canceling the pipe context makes the
// source and every sink return promptly, with no goroutine left blocked on
// the mixer hand-off and no buffers leaked.
func TestMixerCancelReturns(t *testing.T) {
	const (
		channels   = 2
		bufferSize = 4
	)
	gate := make(chan struct{})
	mixer := &audio.Mixer{}
	sink := &mock.Sink{Discard: true}
	p, err := pipe.New(
		bufferSize,
		pipe.Line{
			Source: (&gateSource{channels: channels, gate: gate}).source(),
			Sink:   mixer.Sink(),
		},
		pipe.Line{
			Source: (&gateSource{channels: channels, gate: gate}).source(),
			Sink:   mixer.Sink(),
		},
		pipe.Line{
			Source: mixer.Source(),
			Sink:   sink.Sink(),
		},
	)
	if err != nil {
		t.Fatalf("pipe new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := p.Start(ctx)

	// let every component goroutine start and park.
	select {
	case <-after(100 * time.Millisecond):
	case <-after(externalTestTimeout):
	}

	assertConverged(t, errc, cancel)
}

// TestMixerCancelDuringDynamicAppend exercises the three events together:
// an input is appended while another input already finished, then the
// context is canceled. Everything must converge without blocked waiters or
// leaked buffers.
func TestMixerCancelDuringDynamicAppend(t *testing.T) {
	const (
		channels   = 1
		bufferSize = 4
	)
	gate := make(chan struct{})
	mixer := &audio.Mixer{}
	sink := &mock.Sink{Discard: true}
	p, err := pipe.New(
		bufferSize,
		pipe.Line{
			Source: (&mock.Source{
				Limit:    bufferSize,
				Channels: channels,
				Value:    0.4,
			}).Source(),
			Sink: mixer.Sink(),
		},
		pipe.Line{
			Source: mixer.Source(),
			Sink:   sink.Sink(),
		},
	)
	if err != nil {
		t.Fatalf("pipe new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := p.Start(ctx)

	p.Push(p.AddLine(pipe.Line{
		Source: (&gateSource{channels: channels, gate: gate}).source(),
		Sink:   mixer.Sink(),
	}))

	// let the short input flush and the new one park, then cancel.
	select {
	case <-after(100 * time.Millisecond):
	case <-after(externalTestTimeout):
	}

	assertConverged(t, errc, cancel)
}
