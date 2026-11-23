package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	if m.filePrompt != nil || m.editing {
		return nil
	}
	if m.help {
		if msg.Button == tea.MouseButtonWheelDown {
			m.scrollHelp(3)
		}
		if msg.Button == tea.MouseButtonWheelUp {
			m.scrollHelp(-3)
		}
		return nil
	}
	if m.doc != nil && !m.help && (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		l := m.layout()
		x := 1
		if l.files > 0 {
			x += l.files + 1
		}
		if l.preview > 0 && msg.X >= x && msg.X < x+l.preview && msg.Y >= 3 && msg.Y < 3+l.body {
			delta := 3
			if msg.Button == tea.MouseButtonWheelUp {
				delta = -3
			}
			m.scrollDocument(delta)
			return m.requestDocumentImages()
		}
		return nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft || m.help {
		return nil
	}
	x, y := msg.X, msg.Y
	if y == 1 && x < 22 {
		return m.parent()
	}
	if m.transportVisible() && y == m.height-5 {
		m.timeline = true
		if m.duration > 0 {
			return m.seek(float64(max(0, min(m.width-4, x-2))) / float64(max(1, m.width-4)) * m.duration)
		}
		return nil
	}
	if m.transportVisible() && y == m.height-4 {
		at := 1
		for _, b := range m.buttons() {
			width := ansi.StringWidth(b.label) + 2
			if x >= at && x < at+width {
				return m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(b.key)})
			}
			at += width + 2
		}
		return nil
	}
	l := m.layout()
	row := y - 3
	if row < 0 || row >= l.body {
		return nil
	}
	if l.files > 0 && x >= 1 && x < 1+l.files {
		m.timeline = false
		m.cursor = min(len(m.entries)-1, max(0, m.cursor-l.body+1)+row)
		return m.previewSelection()
	}
	queueX := 1
	if l.files > 0 {
		queueX += l.files + 1
	}
	if l.preview > 0 {
		queueX += l.preview + 1
	}
	if l.queue > 0 && x >= queueX {
		m.timeline = false
		m.queueCursor = min(len(m.queue)-1, max(0, m.queueCursor-l.body+1)+row)
	}
	return nil
}
