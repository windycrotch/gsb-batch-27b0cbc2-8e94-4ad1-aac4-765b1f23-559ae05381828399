package tui

import (
	"fmt"
	"github.com/lancekrogers/peep/internal/document"
	"regexp"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type documentSearch struct {
	query                                     string
	editing                                   bool
	index, origin                             int
	cachedQuery                               string
	cachedLine                                *document.Line
	cachedLength, cachedPreview, cachedImages int
	cachedSource, cached                      bool
	matches                                   []documentMatch
}
type documentMatch struct{ row, start, endRow, end int }

// Search the displayed text, without ANSI styling or native image payloads.
func (m *Model) documentMatches() []documentMatch {
	if m.docSearch.query == "" {
		return nil
	}
	search := &m.docSearch
	var first *document.Line
	if len(m.docLayout.Lines) > 0 {
		first = &m.docLayout.Lines[0]
	}
	if search.cached && search.cachedQuery == search.query && search.cachedLine == first && search.cachedLength == len(m.docLayout.Lines) && search.cachedPreview == m.previewID && search.cachedImages == m.docImageRevision && search.cachedSource == m.docSource {
		return search.matches
	}
	pattern := regexp.MustCompile("(?i)" + regexp.QuoteMeta(search.query))
	lines, _ := m.documentComposition()
	var matches []documentMatch
	for row := 0; row < len(lines); {
		endRow := row + 1
		// Plain/source rows sharing a source line are soft wraps. Join them so
		// a search still finds words split by terminal width.
		if m.doc != nil && (m.docSource || m.doc.Kind != "MD") {
			for endRow < len(lines) && lines[endRow].SourceLine == lines[row].SourceLine {
				endRow++
			}
		}
		var plain strings.Builder
		offsets := []int{0}
		for i := row; i < endRow; i++ {
			plain.WriteString(ansi.Strip(lines[i].Text))
			offsets = append(offsets, plain.Len())
		}
		for _, match := range pattern.FindAllStringIndex(plain.String(), -1) {
			from := sort.Search(len(offsets)-1, func(i int) bool { return offsets[i+1] > match[0] })
			to := sort.Search(len(offsets)-1, func(i int) bool { return offsets[i+1] >= match[1] })
			matches = append(matches, documentMatch{row + from, match[0] - offsets[from], row + to, match[1] - offsets[to]})
		}
		row = endRow
	}
	search.cachedQuery, search.cachedLine, search.cachedLength = search.query, first, len(m.docLayout.Lines)
	search.cachedPreview, search.cachedImages, search.cachedSource, search.cached = m.previewID, m.docImageRevision, m.docSource, true
	search.matches = matches
	return matches
}
func (m *Model) searchDocumentKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		origin := m.docSearch.origin
		m.docSearch = documentSearch{}
		m.docOffset = origin
		m.scrollDocument(0)
		return m.requestDocumentImages()
	case "enter":
		m.docSearch.editing = false
		return nil
	case "ctrl+u":
		m.docSearch.query = ""
	case "backspace":
		runes := []rune(m.docSearch.query)
		if len(runes) > 0 {
			m.docSearch.query = string(runes[:len(runes)-1])
		}
	default:
		if len(k.Runes) == 0 || len(m.docSearch.query)+len(string(k.Runes)) > 1024 {
			return nil
		}
		m.docSearch.query += string(k.Runes)
	}
	matches := m.documentMatches()
	m.docSearch.index = 0
	for i, match := range matches {
		if match.row >= m.docSearch.origin {
			m.docSearch.index = i
			break
		}
	}
	if len(matches) == 0 {
		m.docOffset = m.docSearch.origin
		m.scrollDocument(0)
	}
	return m.revealDocumentMatch(matches)
}
func (m *Model) nextDocumentMatch(delta int) tea.Cmd {
	matches := m.documentMatches()
	if len(matches) == 0 {
		return nil
	}
	m.docSearch.index = (m.docSearch.index + delta + len(matches)) % len(matches)
	return m.revealDocumentMatch(matches)
}
func (m *Model) revealDocumentMatch(matches []documentMatch) tea.Cmd {
	if len(matches) == 0 {
		return nil
	}
	m.docSearch.index = min(max(0, m.docSearch.index), len(matches)-1)
	row := matches[m.docSearch.index].row
	if row < m.docOffset || row >= m.docOffset+m.layout().body {
		m.docOffset = row
		m.scrollDocument(0)
	}
	return m.requestDocumentImages()
}
func (m *Model) documentSearchStatus() string {
	if m.docSearch.query == "" {
		return "Type to search"
	}
	matches := m.documentMatches()
	if len(matches) == 0 {
		return "No matches"
	}
	return fmt.Sprintf("Match %d/%d", min(m.docSearch.index+1, len(matches)), len(matches))
}
func highlightDocumentMatches(text string, row int, matches []documentMatch, current int) string {
	plain := ansi.Strip(text)
	var result strings.Builder
	previous := 0
	found := false
	first := sort.Search(len(matches), func(i int) bool { return matches[i].endRow >= row })
	for i := first; i < len(matches) && matches[i].row <= row; i++ {
		match := matches[i]
		from, to := 0, len(plain)
		if row == match.row {
			from = match.start
		}
		if row == match.endRow {
			to = match.end
		}
		found = true
		start, end := ansi.StringWidth(plain[:from]), ansi.StringWidth(plain[:to])
		result.WriteString(ansi.Cut(text, previous, start))
		shade := "244;192;112"
		if i == current {
			shade = accent
		}
		result.WriteString("\x1b[48;2;" + shade + "m\x1b[38;2;" + bg + "m" + plain[from:to] + "\x1b[0m")
		previous = end
	}
	if !found {
		return text
	}
	result.WriteString(ansi.Cut(text, previous, ansi.StringWidth(text)))
	return result.String()
}
