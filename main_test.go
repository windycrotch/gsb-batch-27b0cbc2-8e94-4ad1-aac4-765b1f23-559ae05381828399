package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezmoss/citychime/internal/chime"
)

// stubSound gates playback on a channel so tests can cancel mid-strike.
type stubSound struct {
	started chan struct{}
	release chan struct{}
}

func (s *stubSound) PlayOnce(ctx context.Context) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type stubLoader struct {
	sound  *stubSound
	loads  atomic.Int32
	closed atomic.Int32
}

func (l *stubLoader) Load() (chime.Sound, error) {
	l.loads.Add(1)
	return l.sound, nil
}

func (l *stubLoader) Close() error {
	l.closed.Add(1)
	return nil
}

func TestSchedulerCancelsPlayingChimeOnShutdown(t *testing.T) {
	loader := &stubLoader{sound: &stubSound{
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}}
	s := chime.New(chime.Options{Loader: loader, Gap: -1})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.Trigger(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("trigger was not accepted")
	}
	select {
	case <-loader.sound.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("chime never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown took %v, want the playing chime interrupted", elapsed)
	}
	if loader.closed.Load() != 1 {
		t.Fatalf("loader closed %d times, want 1", loader.closed.Load())
	}
}

// inTempDir runs the test body with the working directory set to a fresh temp
// dir. (testing.T.Chdir needs Go 1.24 and this module targets 1.22.)
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	return dir
}

func TestFindResourceReportsMissingFileInsteadOfExiting(t *testing.T) {
	_, err := findResource("definitely-not-present-here.mp3")
	if err == nil {
		t.Fatalf("findResource succeeded for a missing file")
	}
	if !strings.Contains(err.Error(), "definitely-not-present-here.mp3") {
		t.Fatalf("error %q does not name the resource", err)
	}
}

func TestFindResourceFindsFileInWorkingDirectory(t *testing.T) {
	dir := inTempDir(t)
	name := "chime-test-resource.tmp"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	path, err := findResource(name)
	if err != nil {
		t.Fatalf("findResource: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("findResource returned an unusable path %q: %v", path, err)
	}
}

func TestLoadIconFallsBackInsteadOfFailing(t *testing.T) {
	inTempDir(t)
	if _, err := loadIcon(); err == nil {
		t.Fatalf("loadIcon succeeded without an icon file")
	}

	icon := fallbackIcon()
	if len(icon) == 0 {
		t.Fatalf("fallbackIcon returned no data")
	}
	if runtime.GOOS == "windows" {
		if len(icon) < 6 || binary.LittleEndian.Uint16(icon[2:4]) != 1 {
			t.Fatalf("fallback icon is not a valid ICO container")
		}
	} else if _, err := png.Decode(bytes.NewReader(icon)); err != nil {
		t.Fatalf("fallback icon is not valid PNG: %v", err)
	}
}

func TestLoadIconReadsExistingFile(t *testing.T) {
	dir := inTempDir(t)
	data := fallbackIcon()
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	icon, err := loadIcon()
	if err != nil {
		t.Fatalf("loadIcon: %v", err)
	}
	if runtime.GOOS == "windows" {
		// PNG input is repackaged so the Windows tray API can load it.
		if !isICO(icon) {
			t.Fatalf("loadIcon returned data that is not an ICO container on Windows")
		}
		return
	}
	if !bytes.Equal(icon, data) {
		t.Fatalf("loadIcon returned different bytes than the icon file")
	}
}

func TestTruncateKeepsShortStringsAndCutsLongOnes(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate(short) = %q", got)
	}
	got := truncate("a fairly long message", 8)
	if len([]rune(got)) != 8 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncate(long) = %q, want 8 runes ending in an ellipsis", got)
	}
}

func TestReporterRecordsProblem(t *testing.T) {
	app := &application{uiWake: make(chan struct{}, 1)}
	app.ReportError(errors.New("audio device unavailable"))

	app.uiMu.Lock()
	problem := app.audioErr
	app.uiMu.Unlock()
	if !strings.Contains(problem, "audio device unavailable") {
		t.Fatalf("audioErr = %q, want the reported message", problem)
	}
	select {
	case <-app.uiWake:
	default:
		t.Fatalf("ReportError did not wake the UI loop")
	}

	app.ReportHealthy()
	app.uiMu.Lock()
	problem = app.audioErr
	app.uiMu.Unlock()
	if problem != "" {
		t.Fatalf("audioErr = %q after recovery, want it cleared", problem)
	}
	select {
	case <-app.uiWake:
	default:
		t.Fatalf("ReportHealthy did not wake the UI loop")
	}
}
