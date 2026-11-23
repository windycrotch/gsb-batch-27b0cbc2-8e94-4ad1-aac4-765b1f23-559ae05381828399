package audio_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mowshon/moviego/v2/audio"
	"github.com/mowshon/moviego/v2/clip"
)

// The cancel/concurrency tests share a BGM-and-narration style graph: a stereo
// 48 kHz bed, a mono voice gated to a mid window by its placement, and a
// 24 kHz source that exercises the resampling (interpolation) path.
func bedGen(i int) float32   { return float32(i%89)/89*0.6 - 0.3 }
func voiceGen(i int) float32 { return float32(i%97)/97*0.5 - 0.25 }
func rampGen(i int) float32  { return float32(i%53)/53*0.4 - 0.2 }

func mixChildren() (bed, voice, ramp audio.AudioClip) {
	bed = newFake(48000, 2, 2*time.Second, bedGen)
	voice = newFake(48000, 1, 700*time.Millisecond, voiceGen).
		WithStart(500 * time.Millisecond).(audio.AudioClip)
	ramp = newFake(24000, 1, 1500*time.Millisecond, rampGen).
		WithStart(250 * time.Millisecond).(audio.AudioClip)
	return bed, voice, ramp
}

func mixGraph() audio.AudioClip {
	bed, voice, ramp := mixChildren()
	return audio.Mix(bed, voice, ramp)
}

const (
	mixTestFrames = 48000 * 2 // 2 s at the mix rate
	mixTestChunk  = 997       // odd chunk, so boundaries land mid-fraction
)

// renderAllErr pulls c over [0, frames) in chunk-sized calls — mirroring
// RenderToTemp's chunked driving — and returns the concatenated samples.
func renderAllErr(c audio.AudioClip, frames, chunk int) ([]float32, error) {
	rate := c.SampleRate()
	ch := c.Channels()
	out := make([]float32, 0, frames*ch)
	buf := &clip.AudioBuffer{}
	for off := 0; off < frames; off += chunk {
		n := min(chunk, frames-off)
		if err := c.SamplesInto(context.Background(), clip.SampleTime(off, rate), n, buf); err != nil {
			return nil, err
		}
		out = append(out, buf.Samples...)
	}
	return out, nil
}

func renderAll(t *testing.T, c audio.AudioClip, frames, chunk int) []float32 {
	t.Helper()
	out, err := renderAllErr(c, frames, chunk)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// requireIdentical compares two renders sample for sample. A retry after
// cancel (or a concurrent render) must match the reference bit for bit — no
// tolerance — because the mix graph is deterministic.
func requireIdentical(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sample count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v (must be bit-identical)", i, got[i], want[i])
		}
	}
}

// poisonChild wraps a clip and, on the fireOn-th pull (1-based), scribbles
// partial junk into dst and cancels the render context — the shape a decoder
// cancellation takes when it dies halfway through filling the target buffer.
type poisonChild struct {
	audio.AudioClip
	cancel context.CancelFunc
	fireOn int
	pulls  int
}

func (c *poisonChild) SamplesInto(ctx context.Context, start clip.Time, count int, dst *clip.AudioBuffer) error {
	c.pulls++
	if c.pulls == c.fireOn {
		dst.Reset(count, c.Channels())
		for i := range dst.Samples {
			dst.Samples[i] = 42
		}
		c.cancel()
		return ctx.Err()
	}
	return c.AudioClip.SamplesInto(ctx, start, count, dst)
}

// pullCounter records how often a child was pulled.
type pullCounter struct {
	audio.AudioClip
	pulls int
}

func (c *pullCounter) SamplesInto(ctx context.Context, start clip.Time, count int, dst *clip.AudioBuffer) error {
	c.pulls++
	return c.AudioClip.SamplesInto(ctx, start, count, dst)
}

// TestMixCancelMidPullStopsChildren verifies a cancel while one child is being
// pulled aborts the whole call with the context error: later children are
// never pulled, and the mix returns an error rather than partial data.
func TestMixCancelMidPullStopsChildren(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	poison := &poisonChild{
		AudioClip: newFake(sr, 1, time.Second, func(int) float32 { return 0.5 }),
		cancel:    cancel,
		fireOn:    1,
	}
	later := &pullCounter{AudioClip: newFake(sr, 1, time.Second, func(int) float32 { return 0.5 })}
	m := audio.Mix(poison, later)

	err := m.SamplesInto(ctx, 0, 1000, &clip.AudioBuffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pull = %v, want context.Canceled", err)
	}
	if later.pulls != 0 {
		t.Fatalf("child after the canceled one was pulled %d times, want 0", later.pulls)
	}
}

// TestMixCancelThenRetryIdentical cancels a chunked render mid-pull — with the
// poisoned child leaving junk in the scratch it was handed — then retries over
// the SAME nodes. The retry must reproduce a never-canceled reference render
// from the first sample on, bit for bit, including across chunk boundaries
// (the odd chunk size lands the resampler mid-fraction at the boundary).
func TestMixCancelThenRetryIdentical(t *testing.T) {
	ref := renderAll(t, mixGraph(), mixTestFrames, mixTestChunk)

	// Cancel mid-render: the poison fires partway through the chunked render.
	ctx, cancel := context.WithCancel(context.Background())
	bed, voice, ramp := mixChildren()
	poison := &poisonChild{AudioClip: bed, cancel: cancel, fireOn: 30}
	graph := audio.Mix(poison, voice, ramp)

	buf := &clip.AudioBuffer{}
	var err error
	for off := 0; off < mixTestFrames && err == nil; off += mixTestChunk {
		n := min(mixTestChunk, mixTestFrames-off)
		err = graph.SamplesInto(ctx, clip.SampleTime(off, 48000), n, buf)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled render = %v, want context.Canceled", err)
	}
	if poison.pulls != 30 {
		t.Fatalf("poison fired on pull %d, want 30", poison.pulls)
	}

	// Retry over the same nodes with a fresh context.
	retry := renderAll(t, graph, mixTestFrames, mixTestChunk)
	requireIdentical(t, retry, ref)
}

// yieldChild yields the scheduler before delegating, so two concurrent renders
// interleave their pulls instead of one running to completion first.
type yieldChild struct{ audio.AudioClip }

func (c yieldChild) SamplesInto(ctx context.Context, start clip.Time, count int, dst *clip.AudioBuffer) error {
	runtime.Gosched()
	return c.AudioClip.SamplesInto(ctx, start, count, dst)
}

// TestMixConcurrentRendersIdentical pulls the same mix graph from two
// goroutines at once (preview + export sharing the nodes). Each render must
// see the complete, correct sample sequence — bit-identical to a solo
// reference — and go test -race must stay clean.
func TestMixConcurrentRendersIdentical(t *testing.T) {
	bed, voice, ramp := mixChildren()
	graph := audio.Mix(yieldChild{bed}, yieldChild{voice}, yieldChild{ramp})

	ref := renderAll(t, graph, mixTestFrames, mixTestChunk)

	start := make(chan struct{})
	results := make([][]float32, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			results[g], errs[g] = renderAllErr(graph, mixTestFrames, mixTestChunk)
		}(g)
	}
	close(start)
	wg.Wait()
	for g := 0; g < 2; g++ {
		if errs[g] != nil {
			t.Fatalf("concurrent render %d: %v", g, errs[g])
		}
		requireIdentical(t, results[g], ref)
	}
}

// TestMixConcurrentWithTransforms routes the children through the transform
// nodes (trim, speed, loop, reverse) that used to keep per-node scratch, and
// runs the same concurrent double render over the graph.
func TestMixConcurrentWithTransforms(t *testing.T) {
	bed, voice, ramp := mixChildren()
	graph := audio.Mix(
		yieldChild{audio.Subclip(bed, 100*time.Millisecond, 1900*time.Millisecond)},
		yieldChild{audio.Speed(voice, 1.5)},
		yieldChild{audio.Loop(ramp, 2)},
		yieldChild{audio.Reverse(newFake(48000, 1, 300*time.Millisecond, rampGen))},
	)

	ref := renderAll(t, graph, mixTestFrames, mixTestChunk)

	start := make(chan struct{})
	results := make([][]float32, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			results[g], errs[g] = renderAllErr(graph, mixTestFrames, mixTestChunk)
		}(g)
	}
	close(start)
	wg.Wait()
	for g := 0; g < 2; g++ {
		if errs[g] != nil {
			t.Fatalf("concurrent render %d: %v", g, errs[g])
		}
		requireIdentical(t, results[g], ref)
	}
}
