package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/browser"
	"github.com/lancekrogers/peep/internal/document"
	"github.com/lancekrogers/peep/internal/media"
)

func TestDocumentFocusAndMediaIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "README.md")
	os.WriteFile(path, []byte("# Read me\n\nText."), 0600)
	m, _ := New(context.Background(), nil, []string{path}, false, "")
	m.entries = []browser.Entry{{Path: path, Name: "README.md"}, {Path: "song.wav", Kind: media.Audio}}
	cmd := m.previewSelection()
	msg := cmd()
	if _, ok := msg.(documentMsg); !ok {
		t.Fatalf("document routed to media: %T", msg)
	}
	m.Update(msg)
	if m.doc == nil {
		t.Fatal(m.previewError)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m.queueFromSelection()
	if len(m.queue) != 0 || m.state != "Empty" {
		t.Fatal("document entered playback")
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.focus {
		t.Fatal("Enter did not focus document")
	}
	for i := 0; i < 100; i++ {
		m.docLayout.Lines = append(m.docLayout.Lines, document.Line{Text: "line", SourceLine: i})
	}
	m.key(tea.KeyMsg{Type: tea.KeyDown})
	if m.docOffset == 0 || m.cursor != 0 {
		t.Fatal("document scroll moved file selection")
	}
	old := m.doc
	m.Update(documentMsg{id: m.previewID - 1, doc: &document.Document{Path: "stale"}})
	if m.doc != old {
		t.Fatal("stale document replaced current")
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus {
		t.Fatal("Esc did not leave focus")
	}
	if _, err := New(context.Background(), nil, []string{path}, true, ""); err == nil {
		t.Fatal("--play accepted document")
	}
}
func TestDocumentSurvivesTrackTransition(t *testing.T) {
	m := imageModel()
	m.doc = &document.Document{Path: "README.md"}
	m.preview = media.Info{Path: "README.md"}
	m.startState = "Playing"
	m.loadID = 2
	m.Update(loadedMsg{id: 2, info: media.Info{Path: "next.png", Kind: media.Image, Duration: 5}})
	if m.doc == nil || m.preview.Path != "README.md" {
		t.Fatal("track transition replaced reading document")
	}
}
