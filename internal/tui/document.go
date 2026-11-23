package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/document"
)

type documentMsg struct {
	id     int
	doc    *document.Document
	layout document.Layout
	anchor int
	err    error
}

func (m *Model) clearDocument() {
	m.cancelDocumentImages()
	m.docImages = nil
	m.doc = nil
	m.docLayout = document.Layout{}
	m.docOffset = 0
	m.docSearch = documentSearch{}
	m.docSource = false
}
func (m *Model) documentWidth() int {
	w := m.layout().preview - 2
	if w < 4 {
		w = m.width - 4
	}
	return max(4, w)
}
func (m *Model) documentAnchor() int {
	lines, _ := m.documentComposition()
	if len(lines) == 0 {
		return 0
	}
	return lines[min(m.docOffset, len(lines)-1)].SourceLine
}
func (m *Model) readDocument(path string) tea.Cmd {
	id, width := m.previewID, m.documentWidth()
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.previewCancel = cancel
	return func() tea.Msg {
		defer cancel()
		d, err := document.Read(ctx, path)
		var layout document.Layout
		if err == nil {
			layout, err = document.Render(ctx, d, width, false)
		}
		return documentMsg{id, d, layout, 0, err}
	}
}
func (m *Model) reflowDocument() tea.Cmd {
	if m.doc == nil {
		return nil
	}
	m.cancelDocumentImages()
	if m.previewCancel != nil {
		m.previewCancel()
	}
	m.previewID++
	id, d, width, raw, anchor := m.previewID, m.doc, m.documentWidth(), m.docSource, m.documentAnchor()
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.previewCancel = cancel
	return func() tea.Msg {
		defer cancel()
		l, e := document.Render(ctx, d, width, raw)
		return documentMsg{id, d, l, anchor, e}
	}
}
func (m *Model) applyDocument(v documentMsg) {
	if v.err != nil {
		m.previewError = v.err.Error()
		return
	}
	m.doc = v.doc
	m.docLayout = v.layout
	m.restoreDocumentAnchor(v.anchor)
}
func (m *Model) scrollDocument(delta int) {
	lines, _ := m.documentComposition()
	m.docOffset = max(0, min(max(0, len(lines)-m.layout().body), m.docOffset+delta))
}
func (m *Model) documentRows(w, h int) []string {
	lines, _ := m.documentComposition()
	rows := make([]string, h)
	matches := m.documentMatches()
	for i := range rows {
		at := m.docOffset + i
		if at < len(lines) {
			rows[i] = highlightDocumentMatches(lines[at].Text, at, matches, m.docSearch.index)
		}
	}
	return rows
}
func (m *Model) documentTitle() string {
	mode := "Text"
	if m.doc.Kind == "MMD" {
		mode = "Mermaid"
	}
	if m.doc.Kind == "MD" || m.doc.Kind == "MMD" {
		if m.doc.Kind == "MD" {
			mode = "Markdown"
		}
		if m.docSource {
			mode = "Source"
		}
	}
	return fmt.Sprintf("%s · %s", mode, m.documentPosition())
}
