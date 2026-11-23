package media

import (
	"bytes"
	"context"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for path, want := range map[string]Kind{"PIC.JPG": Image, "a.GIF": GIF, "movie.webm": Video, "song.flac": Audio, "chapter.M4B": Audio, "notes.txt": ""} {
		if got := Classify(path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}
func TestFixtures(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("set PEEP_INTEGRATION=1 and run just fixtures")
	}
	for _, name := range []string{"01-image.png", "02-animation.gif", "03-video.mp4", "04-audio.wav", "06-photo.jpg"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join("..", "..", "tmp", "fixtures", name)
			info, err := Probe(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Duration <= 0 {
				t.Fatalf("missing duration: %+v", info)
			}
			if info.HasVisual {
				frames := Frames(ctx, path, 60, 36, 0, info.Kind == Image)
				count := 0
				for f := range frames {
					if f.Err != nil {
						t.Fatal(f.Err)
					}
					if len(f.RGB) != 60*36*3 {
						t.Fatal("wrong frame size")
					}
					count++
					if count == 3 {
						cancel()
						break
					}
				}
				if count == 0 {
					t.Fatal("no frames")
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Probe(ctx, "../../tmp/fixtures/05-broken.png"); err == nil {
		t.Fatal("corrupt image accepted")
	}
}
func TestDecodeCancellation(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := Frames(ctx, "../../tmp/fixtures/03-video.mp4", 80, 48, 0, false)
	select {
	case f := <-ch:
		if f.Err != nil {
			t.Fatal(f.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first frame timed out")
	}
	cancel()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("decoder did not terminate")
		}
	}
}

func TestStillDoesNotSeek(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, pos := range []float64{0, 3} {
		f := <-Frames(ctx, "../../tmp/fixtures/06-photo.jpg", 61, 38, pos, true)
		if f.Err != nil || len(f.RGB) != 61*38*3 {
			t.Fatalf("JPEG at slideshow time %v: %v (%d bytes)", pos, f.Err, len(f.RGB))
		}
	}
}

func TestNativePreviewPreservesPixels(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := PreviewPNG(ctx, "../../tmp/fixtures/07-detail.jpg")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1200 || cfg.Height != 800 {
		t.Fatalf("image reduced to %dx%d", cfg.Width, cfg.Height)
	}
	if _, err := PreviewPNG(ctx, "../../tmp/fixtures/05-broken.png"); err == nil {
		t.Fatal("bad native preview accepted")
	}
}

func TestNativeVideoPreservesDetail(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := NativeFrames(ctx, "../../tmp/fixtures/08-detail.mp4", 0)
	f := <-frames
	if f.Err != nil {
		t.Fatal(f.Err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(f.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1280 || cfg.Height != 720 {
		t.Fatalf("video reduced to %dx%d", cfg.Width, cfg.Height)
	}
	second := <-frames
	if second.Err != nil || second.Time <= f.Time {
		t.Fatal("native video clock did not advance")
	}
	cancel()
	for range frames {
	}
}

func TestConfigurableFrameRateAndCancellation(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	for _, fps := range []float64{6, 24} {
		for _, native := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var frames <-chan Frame
			if native {
				frames = NativeFramesAtFPS(ctx, "../../tmp/fixtures/03-video.mp4", 1, fps)
			} else {
				frames = FramesAtFPS(ctx, "../../tmp/fixtures/03-video.mp4", 80, 40, 1, false, fps)
			}
			for i := 0; i < 3; i++ {
				f, ok := <-frames
				if !ok || f.Err != nil {
					t.Fatalf("fps %g native %v: %v", fps, native, f.Err)
				}
				if math.Abs(f.Time-(1+float64(i)/fps)) > 1e-6 {
					t.Fatalf("incorrect FPS timestamp: %v", f.Time)
				}
			}
			cancel()
			for range frames {
			}
		}
	}
}
