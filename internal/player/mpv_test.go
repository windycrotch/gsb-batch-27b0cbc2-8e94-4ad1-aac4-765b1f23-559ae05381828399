package player

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("playback condition timed out")
}
func TestPlayback(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("set PEEP_INTEGRATION=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := New(ctx, "null")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, name := range []string{"03-video.mp4", "04-audio.wav", "02-animation.gif"} {
		t.Run(name, func(t *testing.T) {
			path, _ := filepath.Abs(filepath.Join("../../tmp/fixtures", name))
			if err := p.Load(ctx, path); err != nil {
				t.Fatal(err)
			}
			if err := p.Pause(ctx, false); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return p.Snapshot().Position > 0.2 })
			if err := p.Pause(ctx, true); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return p.Snapshot().Paused })
			pos := p.Snapshot().Position
			time.Sleep(180 * time.Millisecond)
			if math.Abs(p.Snapshot().Position-pos) > 0.12 {
				t.Fatal("paused clock advanced")
			}
			if err := p.Seek(ctx, 1.5); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return math.Abs(p.Snapshot().Position-1.5) < 0.15 })
			if err := p.Seek(ctx, 0); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return p.Snapshot().Position < 0.15 })
		})
	}
	if err := p.Load(ctx, "/nonexistent/peep-missing.mp4"); err == nil {
		t.Fatal("missing file load succeeded")
	}
	dir := p.dir
	p.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("IPC directory not removed")
	}
}
