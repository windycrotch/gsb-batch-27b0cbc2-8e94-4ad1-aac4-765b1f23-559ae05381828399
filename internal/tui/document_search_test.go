package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/browser"
	"github.com/lancekrogers/peep/internal/document"
)

func TestFocusedDocumentSearch(t *testing.T) {
	m := imageModel()
	m.width, m.height, m.focus = 90, 18, true
	m.doc = &document.Document{Path: "note.txt"}
	for i := 0; i < 80; i++ {
		m.docLayout.Lines = append(m.docLayout.Lines, document.Line{Text: "filler"})
	}
	m.docLayout.Lines[30].Text = color("界 Café café [x]", accent)
	m.docLayout.Lines[70].Text = "CAFÉ"
	m.filter = "note"
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.focus || m.filtering || !m.docSearch.editing {
		t.Fatal("search left focused document")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("café")})
	if got := m.documentSearchStatus(); got != "Match 1/3" {
		t.Fatal(got)
	}
	rows := m.documentRows(85, m.layout().body)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "\x1b[48;2;") || !strings.Contains(ansi.Strip(joined), "界 Café café [x]") {
		t.Fatal("highlight altered visible Unicode text", joined)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.docSearch.index != 1 {
		t.Fatal("second occurrence on same row skipped")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.docSearch.index != 2 || m.docOffset+m.layout().body <= 70 {
		t.Fatal("next match not scrolled into view")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.docSearch.index != 0 {
		t.Fatal("next did not wrap")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("N")})
	if m.docSearch.index != 2 {
		t.Fatal("previous did not wrap")
	}
	origin := m.docOffset
	m.key(tea.KeyMsg{Type: tea.KeyCtrlF})
	m.key(tea.KeyMsg{Type: tea.KeyCtrlU})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[x]")})
	if len(m.documentMatches()) != 1 {
		t.Fatal("query treated as regexp")
	}
	m.key(tea.KeyMsg{Type: tea.KeyCtrlU})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.docSearch.query != "q" || m.documentSearchStatus() != "No matches" {
		t.Fatal("q quit or missing query mishandled")
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.docSearch.query != "" || m.docOffset != origin || m.filter != "note" || !m.focus {
		t.Fatal("cancel changed browser state or lost original position")
	}
	m.clearDocument()
	if m.docSearch.query != "" || m.docSearch.editing {
		t.Fatal("new document retained query")
	}
	m.focus = false
	m.key(tea.KeyMsg{Type: tea.KeyCtrlF})
	if !m.filtering {
		t.Fatal("browser filter regressed")
	}
}

func TestSymlinkRowsKeepDistinctColor(t *testing.T) {
	m := imageModel()
	m.entries = []browser.Entry{{Name: "folder", Dir: true}, {Name: "link", Dir: true, Symlink: true}, {Name: "dangling", Symlink: true}}
	rows := m.fileRows(60, 3, false)
	if !strings.Contains(rows[1], "LINK") || !strings.Contains(rows[2], "LINK") || !strings.Contains(rows[1], "38;2;"+fileColor("", "LINK")) {
		t.Fatal(rows)
	}
	if fileColor("", "LINK") == fileColor("", "DIR") {
		t.Fatal("same directory/link color")
	}
}

func TestSortingPreservesSelectionAndFilter(t *testing.T) {
	m := imageModel()
	m.dir = "/files"
	m.allEntries = []browser.Entry{{Name: "rec-a.txt", Path: "/files/rec-a.txt", Metadata: true, Size: 4}, {Name: "rec-b.txt", Path: "/files/rec-b.txt", Metadata: true, Size: 20}, {Name: "other", Path: "/files/other", Metadata: true}}
	m.entries = append([]browser.Entry(nil), m.allEntries...)
	m.filter = "rec-*"
	m.sortKey("s")
	if len(m.entries) != 2 || m.entries[0].Name != "rec-b.txt" || m.selected().Name != "rec-a.txt" || m.filter != "rec-*" {
		t.Fatal(m.entries, m.cursor)
	}
	m.sortKey("r")
	if m.entries[0].Name != "rec-a.txt" || m.selected().Name != "rec-a.txt" {
		t.Fatal(m.entries)
	}
	m.filter = "*.txt"
	m.refilter()
	if !m.sortExplicit || m.sortField != browser.SortSize || len(m.entries) != 2 {
		t.Fatal("filter lost sorting")
	}
}

func TestSearchAcrossSoftWrapAndReflow(t *testing.T) {
	m := imageModel()
	m.doc = &document.Document{Kind: "TEXT"}
	m.docSearch.query = "terminal"
	m.docLayout.Lines = []document.Line{{Text: "the term", SourceLine: 0}, {Text: "inal", SourceLine: 0}, {Text: "term", SourceLine: 1}, {Text: "inal", SourceLine: 2}}
	matches := m.documentMatches()
	if len(matches) != 1 || matches[0].row != 0 || matches[0].endRow != 1 {
		t.Fatal(matches)
	}
	for row := 0; row < 2; row++ {
		highlighted := highlightDocumentMatches(m.docLayout.Lines[row].Text, row, matches, 0)
		if ansi.Strip(highlighted) != m.docLayout.Lines[row].Text || !strings.Contains(highlighted, "48;2;") {
			t.Fatal(highlighted)
		}
	}
	m.docLayout.Lines = []document.Line{{Text: "the terminal", SourceLine: 0}}
	matches = m.documentMatches()
	if len(matches) != 1 || matches[0].endRow != 0 {
		t.Fatal("cached stale layout", matches)
	}
}
