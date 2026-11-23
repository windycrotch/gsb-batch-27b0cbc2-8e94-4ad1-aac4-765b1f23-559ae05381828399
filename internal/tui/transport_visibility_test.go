package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/browser"
	"github.com/lancekrogers/peep/internal/document"
	"github.com/lancekrogers/peep/internal/media"
	"strings"
	"testing"
)

func TestTransportFollowsPlayableContent(t *testing.T) {
	m := imageModel()
	mediaBody := m.layout().body
	m.active = media.Info{}
	m.state = "Empty"
	m.doc = &document.Document{Path: "README.md", Kind: "MD"}
	m.preview = media.Info{Path: "README.md"}
	m.focus = true
	v := ansi.Strip(m.View())
	for _, text := range []string{"NOW PLAYING", "Vol ", "[Empty]", "timeline", "seek step", "Stop s"} {
		if strings.Contains(v, text) {
			t.Fatalf("document retained %q", text)
		}
	}
	if m.layout().body != mediaBody+4 {
		t.Fatal("document did not reclaim transport rows")
	}
	if len(strings.Split(v, "\n")) != m.height {
		t.Fatal("view no longer fills terminal")
	}
	m.key(tea.KeyMsg{Type: tea.KeyTab})
	if m.timeline {
		t.Fatal("invisible timeline received focus")
	}
	// These coordinates used to be transport targets; they're now document rows.
	m.mouse(tea.MouseMsg{X: 30, Y: m.height - 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.timeline {
		t.Fatal("hidden timeline intercepted click")
	}
	m.active = media.Info{Path: "song.flac", Kind: media.Audio}
	m.state = "Playing"
	if !strings.Contains(ansi.Strip(m.View()), "NOW PLAYING") || m.layout().body != mediaBody {
		t.Fatal("background playback lost controls")
	}
	for _, state := range []string{"Ended", "Stopped", "Error"} {
		m.state = state
		if m.transportVisible() {
			t.Fatalf("stale %s controls on document", state)
		}
	}
	m.state = "Paused"
	if !m.transportVisible() {
		t.Fatal("paused background media lost controls")
	}
	m.active = media.Info{}
	m.doc = nil
	for _, kind := range []media.Kind{media.Image, media.GIF, media.Video, media.Audio} {
		m.preview = media.Info{Path: "file", Kind: kind}
		if !m.transportVisible() {
			t.Fatalf("missing controls for %s", kind)
		}
	}
	m.entries = []browser.Entry{{Path: "folder", Dir: true}}
	m.cursor = 0
	m.timeline = true
	m.previewSelection()
	if m.timeline || m.transportVisible() {
		t.Fatal("folder retained empty transport")
	}
}
