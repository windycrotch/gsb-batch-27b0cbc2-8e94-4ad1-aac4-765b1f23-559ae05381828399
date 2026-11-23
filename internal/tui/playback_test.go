package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/media"
)

// fakeDecoder records decoder starts and contexts so tests can verify that
// seeks, file switches, and cancellation replace the frame stream correctly.
type fakeDecoder struct {
	mu     sync.Mutex
	starts []float64
	paths  []string
	ctxs   []context.Context
}

func (f *fakeDecoder) begin(ctx context.Context, path string, start float64) <-chan media.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, start)
	f.paths = append(f.paths, path)
	f.ctxs = append(f.ctxs, ctx)
	return make(chan media.Frame)
}

func (f *fakeDecoder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}
func (f *fakeDecoder) startAt(i int) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[i]
}
func (f *fakeDecoder) pathAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[i]
}
func (f *fakeDecoder) cancelled(i int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.ctxs[i].Done():
		return true
	default:
		return false
	}
}

func installFakeDecoder(t *testing.T) *fakeDecoder {
	t.Helper()
	f := &fakeDecoder{}
	oldCells, oldNative := decodeFramesAtFPS, decodeNativeFramesAtFPS
	decodeFramesAtFPS = func(ctx context.Context, path string, width, height int, start float64, single bool, fps float64) <-chan media.Frame {
		return f.begin(ctx, path, start)
	}
	decodeNativeFramesAtFPS = func(ctx context.Context, path string, start, fps float64) <-chan media.Frame {
		return f.begin(ctx, path, start)
	}
	t.Cleanup(func() { decodeFramesAtFPS, decodeNativeFramesAtFPS = oldCells, oldNative })
	return f
}

func TestSeekRestartsDecoderAtTargetAndDropsOldFrames(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.state = "Playing"
	m.position = 10
	m.showActive()
	if f.count() != 1 || f.startAt(0) != 10 {
		t.Fatalf("initial decoder start: %v", f.starts)
	}
	oldID := m.previewID
	pos := 42.0
	m.Update(controlMsg{seek: &pos})
	if m.position != 42 {
		t.Fatal("seek target not applied to the clock")
	}
	if f.count() != 2 || f.startAt(1) != 42 {
		t.Fatalf("decoder not restarted at seek target: %v", f.starts)
	}
	if !f.cancelled(0) {
		t.Fatal("pre-seek decoder was not cancelled")
	}
	if m.previewID == oldID {
		t.Fatal("preview generation not advanced")
	}
	m.Update(frameMsg{id: oldID, ok: true, frame: media.Frame{Time: 42, PNG: []byte("old decoder")}})
	if string(m.nativePNG) == "old decoder" {
		t.Fatal("stale decoder frame overwrote the picture")
	}
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 42, PNG: []byte("fresh")}})
	if string(m.nativePNG) != "fresh" {
		t.Fatal("frame from the new decoder was not presented")
	}
}

func TestSeekBackwardWhilePlayingDoesNotDropFreshFrames(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.state = "Playing"
	m.position = 50
	m.showActive()
	pos := 10.0
	m.Update(controlMsg{seek: &pos})
	if f.count() != 2 || f.startAt(1) != 10 {
		t.Fatalf("decoder not restarted at seek target: %v", f.starts)
	}
	// Frames from the fresh decoder are near the new clock and must be shown,
	// not mistaken for a stale backlog from before the seek.
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 10, PNG: []byte("fresh")}})
	if string(m.nativePNG) != "fresh" {
		t.Fatal("fresh frame dropped after backward seek")
	}
}

func TestPausedSeekShowsTargetFrameWithoutReplayingPrefetch(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.state = "Paused"
	m.position = 10
	m.showActive()
	pos := 20.0
	m.Update(controlMsg{seek: &pos})
	if f.count() != 2 || f.startAt(1) != 20 {
		t.Fatalf("decoder not restarted at seek target: %v", f.starts)
	}
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 20, PNG: []byte("target")}})
	if string(m.nativePNG) != "target" {
		t.Fatal("paused seek target frame not shown")
	}
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 20.5, PNG: []byte("prefetch")}})
	if string(m.nativePNG) != "target" {
		t.Fatal("paused video presented prefetched frames")
	}
	if m.pending == nil {
		t.Fatal("future frame was not held while paused")
	}
}

func TestFileSwitchReplacesDecoderAndRejectsOldFrames(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.state = "Playing"
	m.position = 7
	m.showActive()
	oldID := m.previewID
	// Mimic loadState + a completed load of the next queue entry.
	m.position = 0
	m.duration = 0
	m.active = media.Info{Path: "next.mp4"}
	m.startState = "Playing"
	m.Update(loadedMsg{id: m.loadID, info: media.Info{Path: "next.mp4", Kind: media.Video, HasVisual: true, Duration: 60}})
	if f.count() != 2 || f.pathAt(1) != "next.mp4" || f.startAt(1) != 0 {
		t.Fatalf("file switch did not start a fresh decoder: %v %v", f.paths, f.starts)
	}
	if !f.cancelled(0) {
		t.Fatal("previous file decoder was not cancelled")
	}
	m.Update(frameMsg{id: oldID, ok: true, frame: media.Frame{Time: 7.2, PNG: []byte("previous file")}})
	if string(m.nativePNG) == "previous file" {
		t.Fatal("previous file frame overwrote the new picture")
	}
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 0, PNG: []byte("new file")}})
	if string(m.nativePNG) != "new file" {
		t.Fatal("new file frame not presented")
	}
}

func TestResumePresentsHeldFramesInOrder(t *testing.T) {
	m := videoModel()
	m.state = "Paused"
	m.position = 5
	// Prefetch decoded while paused must wait for the clock, not replay.
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 5.1, PNG: []byte("f1")}})
	if m.pending == nil || string(m.nativePNG) != "current" {
		t.Fatal("paused video presented prefetched frames")
	}
	m.state = "Playing"
	m.position = 5.1
	m.Update(tickMsg(time.Now()))
	if m.pending != nil || string(m.nativePNG) != "f1" {
		t.Fatal("held frame not presented in order on resume")
	}
}

func TestActiveDecodeFailureKeepsPictureAndShowsNotice(t *testing.T) {
	m := videoModel()
	m.frames = make(chan media.Frame)
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Err: errors.New("decoder exploded")}})
	if string(m.nativePNG) != "current" {
		t.Fatal("decode failure cleared the last good frame")
	}
	if m.notice == "" || m.previewError == "" {
		t.Fatal("decode failure left no visible notice")
	}
	if m.frames != nil {
		t.Fatal("failed decoder was not stopped")
	}
}

func TestQuitCancelsDecoder(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.showActive()
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !f.cancelled(0) {
		t.Fatal("quit did not cancel the decoder")
	}
}

func TestSelectionChangeCancelsDecoder(t *testing.T) {
	m := videoModel()
	f := installFakeDecoder(t)
	m.showActive()
	m.displayingActive = false
	m.entries = nil
	m.cursor = 0
	m.previewSelection()
	if !f.cancelled(0) {
		t.Fatal("selection change did not cancel the decoder")
	}
}
