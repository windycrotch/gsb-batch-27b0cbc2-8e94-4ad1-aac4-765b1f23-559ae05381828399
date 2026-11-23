package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The tests in this file run a fake ffmpeg: a copy of the test binary placed
// on PATH. The child recognizes PEEP_FAKE_FFMPEG in init and impersonates a
// decoder, so no real ffmpeg, terminal, or network is needed.
func init() {
	if mode := os.Getenv("PEEP_FAKE_FFMPEG"); mode != "" {
		os.Exit(fakeFFmpeg(mode))
	}
}

func fakeFFmpeg(mode string) int {
	width, _ := strconv.Atoi(os.Getenv("PEEP_FAKE_W"))
	height, _ := strconv.Atoi(os.Getenv("PEEP_FAKE_H"))
	count, _ := strconv.Atoi(os.Getenv("PEEP_FAKE_N"))
	if width <= 0 {
		width = 4
	}
	if height <= 0 {
		height = 2
	}
	if count <= 0 {
		count = 3
	}
	frame := make([]byte, width*height*3)
	for i := range frame {
		frame[i] = byte(i)
	}
	var pngFrame []byte
	if mode == "pngstream" || mode == "pnginfinite" {
		var buf bytes.Buffer
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		if err := png.Encode(&buf, img); err != nil {
			fmt.Fprintln(os.Stderr, "png encode:", err)
			return 1
		}
		pngFrame = buf.Bytes()
	}
	write := func() bool {
		var err error
		if pngFrame != nil {
			_, err = os.Stdout.Write(pngFrame)
		} else {
			_, err = os.Stdout.Write(frame)
		}
		return err == nil
	}
	switch mode {
	case "stream", "pngstream":
		for i := 0; i < count; i++ {
			if !write() {
				return 1
			}
		}
		return 0
	case "infinite", "pnginfinite":
		for write() {
			time.Sleep(5 * time.Millisecond)
		}
		return 1
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	case "error":
		fmt.Fprintln(os.Stderr, "boom: invalid stream")
		return 1
	case "empty":
		return 0
	}
	return 2
}

// installFakeFFmpeg puts the fake decoder on PATH and returns its directory.
func installFakeFFmpeg(t *testing.T, mode string, env map[string]string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PEEP_FAKE_FFMPEG", mode)
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func recvFrame(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("frame channel closed early")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frame")
		return Frame{}
	}
}

func expectClosed(t *testing.T, ch <-chan Frame) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("decoder did not terminate")
		}
	}
}

func TestFramesStreamTimestamps(t *testing.T) {
	installFakeFFmpeg(t, "stream", map[string]string{"PEEP_FAKE_W": "4", "PEEP_FAKE_H": "2", "PEEP_FAKE_N": "4"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := FramesAtFPS(ctx, "clip.mp4", 4, 2, 1.5, false, 8)
	for i := 0; i < 4; i++ {
		f := recvFrame(t, ch)
		if f.Err != nil {
			t.Fatal(f.Err)
		}
		want := 1.5 + float64(i)/8
		if f.Time != want {
			t.Fatalf("frame %d timestamp %v, want %v", i, f.Time, want)
		}
		if len(f.RGB) != 4*2*3 {
			t.Fatalf("frame %d payload %d bytes", i, len(f.RGB))
		}
	}
	expectClosed(t, ch)
}

func TestFramesCancellationReapsDecoder(t *testing.T) {
	installFakeFFmpeg(t, "infinite", nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := FramesAtFPS(ctx, "clip.mp4", 4, 2, 0, false, 12)
	if f := recvFrame(t, ch); f.Err != nil {
		t.Fatal(f.Err)
	}
	cancel()
	// The channel closes only after cmd.Wait reaps the killed ffmpeg process,
	// so a closed channel proves the decoder child is gone.
	expectClosed(t, ch)
}

func TestFramesHungDecoderCancelled(t *testing.T) {
	installFakeFFmpeg(t, "hang", nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := FramesAtFPS(ctx, "clip.mp4", 4, 2, 0, false, 12)
	cancel()
	expectClosed(t, ch)
}

func TestNativeFramesCancellationReapsDecoder(t *testing.T) {
	installFakeFFmpeg(t, "pnginfinite", map[string]string{"PEEP_FAKE_W": "4", "PEEP_FAKE_H": "4"})
	ctx, cancel := context.WithCancel(context.Background())
	ch := NativeFramesAtFPS(ctx, "clip.mp4", 0, 12)
	f := recvFrame(t, ch)
	if f.Err != nil {
		t.Fatal(f.Err)
	}
	if !bytes.HasPrefix(f.PNG, []byte{137, 80, 78, 71}) {
		t.Fatal("invalid PNG frame")
	}
	cancel()
	expectClosed(t, ch)
}

func TestNativeFramesStreamTimestamps(t *testing.T) {
	installFakeFFmpeg(t, "pngstream", map[string]string{"PEEP_FAKE_W": "4", "PEEP_FAKE_H": "4", "PEEP_FAKE_N": "3"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := NativeFramesAtFPS(ctx, "clip.mp4", 2, 10)
	for i := 0; i < 3; i++ {
		f := recvFrame(t, ch)
		if f.Err != nil {
			t.Fatal(f.Err)
		}
		want := 2 + float64(i)/10
		if f.Time != want {
			t.Fatalf("frame %d timestamp %v, want %v", i, f.Time, want)
		}
		if f.Width != 4 || f.Height != 4 {
			t.Fatalf("frame %d dimensions %dx%d", i, f.Width, f.Height)
		}
	}
	expectClosed(t, ch)
}

func TestFramesDecodeErrorSurfaces(t *testing.T) {
	installFakeFFmpeg(t, "error", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := recvFrame(t, FramesAtFPS(ctx, "broken.mp4", 4, 2, 0, false, 12))
	if f.Err == nil {
		t.Fatal("decode failure not reported")
	}
	if got := f.Err.Error(); !bytes.Contains([]byte(got), []byte("boom")) {
		t.Fatalf("decoder stderr missing from error: %v", f.Err)
	}
}

func TestFramesEmptyStreamReportsNoVisual(t *testing.T) {
	installFakeFFmpeg(t, "empty", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := recvFrame(t, FramesAtFPS(ctx, "silent.mp4", 4, 2, 0, false, 12))
	if f.Err == nil {
		t.Fatal("empty stream not reported")
	}
}
