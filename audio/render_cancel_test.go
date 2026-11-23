package audio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mowshon/moviego/v2/audio"
	"github.com/mowshon/moviego/v2/clip"
	"github.com/mowshon/moviego/v2/internal/genmedia"
)

// cancelAtPull cancels the render context when its fireOn-th pull (1-based)
// arrives, so the cancellation lands deterministically mid-render.
type cancelAtPull struct {
	audio.AudioClip
	fireOn int
	cancel context.CancelFunc
	pulls  int
}

func (c *cancelAtPull) SamplesInto(ctx context.Context, start clip.Time, count int, dst *clip.AudioBuffer) error {
	c.pulls++
	if c.pulls == c.fireOn {
		c.cancel()
	}
	return c.AudioClip.SamplesInto(ctx, start, count, dst)
}

// openMixFixture builds the BGM-and-narration graph on real files: a stereo
// 44.1 kHz bed and a mono 22.05 kHz voice placed at 1s (different rates, a
// mono up-mix, and a gated window — the resampling path included).
func openMixFixture(t *testing.T, dir string) audio.AudioClip {
	t.Helper()
	bedPath, err := genmedia.SineAudio(dir, "bed.wav", 220, 3.0, 44100)
	if err != nil {
		t.Fatalf("gen bed: %v", err)
	}
	voicePath, err := genmedia.SineAudio(dir, "voice.wav", 660, 1.5, 22050)
	if err != nil {
		t.Fatalf("gen voice: %v", err)
	}
	ctx := context.Background()
	bed, err := audio.OpenFile(ctx, bedPath)
	if err != nil {
		t.Fatalf("open bed: %v", err)
	}
	t.Cleanup(func() { _ = bed.Close() })
	voiceFile, err := audio.OpenFile(ctx, voicePath)
	if err != nil {
		t.Fatalf("open voice: %v", err)
	}
	t.Cleanup(func() { _ = voiceFile.Close() })
	voice := voiceFile.WithStart(time.Second).(audio.AudioClip)
	return audio.Mix(bed, voice)
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestRenderToTempCancelRetryIdentical renders the mix to PCM, cancels a
// second render mid-render, then retries over the SAME nodes: the retry must
// be byte-identical to the never-canceled reference — no leading silence, no
// level or channel drift. PCM output is deterministic, so the comparison is
// exact.
func TestRenderToTempCancelRetryIdentical(t *testing.T) {
	if !genmedia.Available() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	dir := t.TempDir()
	root := openMixFixture(t, dir)

	ref := filepath.Join(dir, "ref.wav")
	if err := audio.RenderToTemp(context.Background(), root, ref, "pcm_s16le", "", 3*time.Second); err != nil {
		t.Fatalf("reference render: %v", err)
	}

	// Cancel mid-render: the first pull cancels the context, so the render
	// aborts inside the first chunk.
	ctx, cancel := context.WithCancel(context.Background())
	canceling := &cancelAtPull{AudioClip: root, fireOn: 1, cancel: cancel}
	err := audio.RenderToTemp(ctx, canceling, filepath.Join(dir, "canceled.wav"), "pcm_s16le", "", 3*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled render = %v, want context.Canceled", err)
	}

	// Retry over the same nodes with a fresh context.
	retry := filepath.Join(dir, "retry.wav")
	if err := audio.RenderToTemp(context.Background(), root, retry, "pcm_s16le", "", 3*time.Second); err != nil {
		t.Fatalf("retry render: %v", err)
	}
	refBytes, retryBytes := readFileBytes(t, ref), readFileBytes(t, retry)
	if len(refBytes) != len(retryBytes) {
		t.Fatalf("retry size = %d, want %d (reference)", len(retryBytes), len(refBytes))
	}
	for i := range refBytes {
		if refBytes[i] != retryBytes[i] {
			t.Fatalf("retry byte %d = %d, want %d (reference)", i, retryBytes[i], refBytes[i])
		}
	}
}

// TestRenderToTempConcurrentRendersIdentical runs two renders of the same
// nodes at once (preview + export). Both outputs must be byte-identical to a
// solo reference render, and go test -race must stay clean.
func TestRenderToTempConcurrentRendersIdentical(t *testing.T) {
	if !genmedia.Available() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	dir := t.TempDir()
	root := openMixFixture(t, dir)

	ref := filepath.Join(dir, "ref.wav")
	if err := audio.RenderToTemp(context.Background(), root, ref, "pcm_s16le", "", 3*time.Second); err != nil {
		t.Fatalf("reference render: %v", err)
	}
	refBytes := readFileBytes(t, ref)

	start := make(chan struct{})
	outs := []string{filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			errs[g] = audio.RenderToTemp(context.Background(), root, outs[g], "pcm_s16le", "", 3*time.Second)
		}(g)
	}
	close(start)
	wg.Wait()
	for g := 0; g < 2; g++ {
		if errs[g] != nil {
			t.Fatalf("concurrent render %d: %v", g, errs[g])
		}
		got := readFileBytes(t, outs[g])
		if len(got) != len(refBytes) {
			t.Fatalf("render %d size = %d, want %d (reference)", g, len(got), len(refBytes))
		}
		for i := range got {
			if got[i] != refBytes[i] {
				t.Fatalf("render %d byte %d = %d, want %d (reference)", g, i, got[i], refBytes[i])
			}
		}
	}
}
