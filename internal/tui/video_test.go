package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/media"
	"github.com/lancekrogers/peep/internal/render"
)

func videoModel() *Model {
	m := imageModel()
	m.active = media.Info{Path: "video.mp4", Kind: media.Video, HasVisual: true}
	m.preview = m.active
	m.duration = 100
	m.displayingActive = true
	m.graphics = &render.KittyWriter{}
	m.nativePNG = []byte("current")
	m.videoStarted = time.Now()
	return m
}
func TestVideoSkipsStaleFramesAndHoldsFutureFrames(t *testing.T) {
	m := videoModel()
	m.position = 5
	for _, at := range []float64{1, 2, 4} {
		m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: at, PNG: []byte("stale")}})
		if string(m.nativePNG) != "current" {
			t.Fatal("presented an overdue frame")
		}
	}
	m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 5.5, PNG: []byte("future")}})
	if m.pending == nil || string(m.nativePNG) != "current" {
		t.Fatal("future frame was not held")
	}
	m.position = 5.5
	m.Update(tickMsg(time.Now()))
	if m.pending != nil || string(m.nativePNG) != "future" {
		t.Fatal("due frame not presented")
	}
}
func TestVideoDropsPendingFrameAfterClockAdvances(t *testing.T) {
	m := videoModel()
	m.pending = &media.Frame{Time: 2, PNG: []byte("stale")}
	m.position = 4
	m.Update(tickMsg(time.Now()))
	if m.pending != nil || string(m.nativePNG) != "current" {
		t.Fatal("pending backlog was presented")
	}
}
func TestVideoResyncKeepsPictureAndInvalidatesOldDecoder(t *testing.T) {
	m := videoModel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.ctx = ctx
	m.position = 10
	m.videoStarted = time.Now().Add(-3 * time.Second)
	id := m.previewID
	_, cmd := m.Update(frameMsg{id: id, ok: true, frame: media.Frame{Time: 1, PNG: []byte("stale")}})
	if cmd == nil || m.previewID == id || string(m.nativePNG) != "current" {
		t.Fatal("resync did not retain picture and replace decoder")
	}
	if m.previewCancel != nil {
		m.previewCancel()
	}
	m.Update(frameMsg{id: id, ok: true, frame: media.Frame{Time: 10, PNG: []byte("old decoder")}})
	if string(m.nativePNG) != "current" {
		t.Fatal("replaced decoder changed image")
	}
}
func TestFPSControlsDoNotInterceptTextOrChangeAudio(t *testing.T) {
	m := videoModel()
	m.displayingActive = false
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	if m.videoFPS() != 24 || !strings.Contains(m.videoFPSLabel(), "24 FPS target") {
		t.Fatal("FPS shortcut or label missing")
	}
	m.filtering = true
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	if m.filter != "F" || m.videoFPS() != 24 {
		t.Fatal("FPS shortcut intercepted filter")
	}
	m.filtering = false
	m.active.Kind = media.Audio
	m.cycleVideoFPS()
	if m.videoFPS() != 24 || m.videoFPSLabel() != "" {
		t.Fatal("audio exposed video controls")
	}
	m.active.Kind = media.Video
	m.SetVideoFPS(60)
	m.cycleVideoFPS()
	if m.videoFPS() != 1 {
		t.Fatal("FPS choices do not wrap")
	}
}

func TestPausedOrEndedVideoDoesNotReplayBacklog(t *testing.T) {
	for _, state := range []string{"Paused", "Ended"} {
		m := videoModel()
		m.state, m.position = state, 4
		m.Update(frameMsg{id: m.previewID, ok: true, frame: media.Frame{Time: 1, PNG: []byte("stale")}})
		m.pending = &media.Frame{Time: 2, PNG: []byte("stale")}
		m.Update(tickMsg(time.Now()))
		if m.pending != nil || string(m.nativePNG) != "current" {
			t.Fatalf("%s replayed a stale frame", state)
		}
	}
}
