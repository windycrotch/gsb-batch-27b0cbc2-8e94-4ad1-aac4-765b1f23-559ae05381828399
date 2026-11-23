package audio

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mowshon/moviego/v2/clip"
	"github.com/mowshon/moviego/v2/internal/genmedia"
)

// TestFileNodeDropsDecoderOnCancel pulls from a file node with a context that
// is canceled while the decode is in flight. The failed pull must drop the
// decoder on the error path itself — killing the FFmpeg process and releasing
// its buffers — instead of leaving it for the next pull or Close.
func TestFileNodeDropsDecoderOnCancel(t *testing.T) {
	if !genmedia.Available() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	path, err := genmedia.SineAudio(t.TempDir(), "long.wav", 440, 30.0, 44100)
	if err != nil {
		t.Fatalf("gen sine: %v", err)
	}
	n := NewFileNode(path, 44100, 1, 30*time.Second)
	defer n.Close()

	// A first successful pull leaves a live decoder bound to its context.
	if err := n.SamplesInto(context.Background(), 0, 100, &clip.AudioBuffer{}); err != nil {
		t.Fatalf("warm-up pull: %v", err)
	}
	if n.r.dec == nil {
		t.Fatal("expected a live decoder after a successful pull")
	}

	// Cancel while a large pull is decoding: the read fails mid-stream.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err = n.SamplesInto(ctx, 0, 44100*25, &clip.AudioBuffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pull = %v, want context.Canceled", err)
	}
	if n.r.dec != nil {
		t.Fatal("canceled pull kept its decoder; want it dropped on the error path")
	}

	// The node still serves a retry from a clean decoder.
	if err := n.SamplesInto(context.Background(), 0, 100, &clip.AudioBuffer{}); err != nil {
		t.Fatalf("retry pull after cancel: %v", err)
	}
}

// TestFileNodeRetryAfterCancelMatchesFresh renders the same node before and
// after a canceled pull and checks the retry is sample-identical to a fresh
// node that was never canceled.
func TestFileNodeRetryAfterCancelMatchesFresh(t *testing.T) {
	if !genmedia.Available() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	path, err := genmedia.SineAudio(t.TempDir(), "sine.wav", 440, 30.0, 44100)
	if err != nil {
		t.Fatalf("gen sine: %v", err)
	}
	used := NewFileNode(path, 44100, 1, 30*time.Second)
	defer used.Close()
	fresh := NewFileNode(path, 44100, 1, 30*time.Second)
	defer fresh.Close()

	// Cancel a pull mid-decode on the used node.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_ = used.SamplesInto(ctx, 0, 44100*25, &clip.AudioBuffer{})

	// Pull the same window from both nodes; the samples must match exactly.
	const n = 44100
	a, b := &clip.AudioBuffer{}, &clip.AudioBuffer{}
	if err := used.SamplesInto(context.Background(), 0, n, a); err != nil {
		t.Fatalf("retry pull: %v", err)
	}
	if err := fresh.SamplesInto(context.Background(), 0, n, b); err != nil {
		t.Fatalf("fresh pull: %v", err)
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			t.Fatalf("sample %d after cancel = %v, want %v (fresh node)", i, a.Samples[i], b.Samples[i])
		}
	}
}
