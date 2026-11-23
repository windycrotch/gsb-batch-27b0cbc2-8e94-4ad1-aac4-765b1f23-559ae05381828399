package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/browser"
	"github.com/lancekrogers/peep/internal/document"
	"strings"
	"testing"
)

func navType(m *Model, s string) {
	for _, r := range s {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}
func TestNumberedNavigation(t *testing.T) {
	m := imageModel()
	for i := 0; i < 60; i++ {
		m.entries = append(m.entries, browser.Entry{Name: fmt.Sprintf("folder-%02d", i), Path: fmt.Sprintf("/folder-%02d", i), Dir: true})
	}
	navType(m, "12G")
	if m.cursor != 11 {
		t.Fatal(m.cursor)
	}
	navType(m, "5j")
	if m.cursor != 16 {
		t.Fatal(m.cursor)
	}
	navType(m, "3k")
	if m.cursor != 13 {
		t.Fatal(m.cursor)
	}
	navType(m, "gg")
	if m.cursor != 0 {
		t.Fatal(m.cursor)
	}
	navType(m, "G")
	if m.cursor != len(m.entries)-1 {
		t.Fatal(m.cursor)
	}
	navType(m, "999999999999999999999j")
	if m.cursor != len(m.entries)-1 {
		t.Fatal("count overflow")
	}
	navType(m, "12gg")
	if m.cursor != 11 {
		t.Fatal(m.cursor)
	}
	navType(m, "9")
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	navType(m, "j")
	if m.cursor != 12 {
		t.Fatal("cancel leaked count")
	}
	navType(m, "#")
	rows := m.fileRows(70, 30, false)
	if !strings.Contains(ansi.Strip(rows[12]), "13 ") || !strings.Contains(ansi.Strip(rows[11]), "1 ") {
		t.Fatal("hybrid numbers", rows[12], rows[11])
	}
	m.filtering = true
	navType(m, "12gg")
	if m.filter != "12gg" {
		t.Fatal("search intercepted", m.filter)
	}
	m.filtering = false
	m.filter = ""
	m.showQueue = true
	m.queue = []string{"a", "b", "c"}
	navType(m, "G")
	if m.queueCursor != 2 {
		t.Fatal("queue jump")
	}
	navType(m, "gg")
	if m.queueCursor != 0 {
		t.Fatal("queue top")
	}
	m.showQueue = false
	m.focus = true
	m.doc = &document.Document{Kind: "TEXT"}
	m.docLayout.Lines = make([]document.Line, 100)
	navType(m, "12G")
	if m.docOffset != 11 {
		t.Fatal("document jump", m.docOffset)
	}
	navType(m, "gg")
	if m.docOffset != 0 {
		t.Fatal("document top")
	}
	m.key(tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.docOffset != m.layout().body/2 {
		t.Fatal("half page")
	}
	m.entries = nil
	m.doc = nil
	m.focus = false
	navType(m, "Ggg")
	if m.cursor != 0 {
		t.Fatal("empty list")
	}
}
func TestFileTypeColorsAndWidths(t *testing.T) {
	m := imageModel()
	m.entries = []browser.Entry{{Name: "folder", Path: "/folder", Dir: true}, {Name: "main.go", Path: "/main.go"}, {Name: "config.yaml", Path: "/config.yaml"}}
	rows := m.fileRows(40, 3, false)
	for _, row := range rows {
		if ansi.StringWidth(row) > 40 {
			t.Fatal("wide row", ansi.Strip(row))
		}
	}
	for i, shade := range []string{accent, "128;188;245", "244;192;112"} {
		if !strings.Contains(rows[i], "38;2;"+shade+"m") {
			t.Fatal("missing type color")
		}
	}
}
