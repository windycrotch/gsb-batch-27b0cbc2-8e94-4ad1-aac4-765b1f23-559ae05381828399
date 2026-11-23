package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) navigationPrefix() string {
	s := ""
	if m.navCount > 0 {
		s = fmt.Sprint(m.navCount)
	}
	if m.navG {
		s += "g"
	}
	if s != "" {
		return " " + s
	}
	return ""
}

// Counts belong only to navigation, never search input or the transport timeline.
func (m *Model) navigationKey(key string) (bool, tea.Cmd) {
	if len(key) == 1 && key[0] >= '0' && key[0] <= '9' && (key != "0" || m.navCount > 0) {
		m.navCount = min(1000000, m.navCount*10+int(key[0]-'0'))
		return true, nil
	}
	count, pending := m.navCount, m.navG
	if key == "g" && !pending {
		m.navG = true
		return true, nil
	}
	m.navCount, m.navG = 0, false
	if key == "esc" && (count > 0 || pending) {
		return true, nil
	}
	delta, target, absolute := 0, 0, false
	switch key {
	case "g":
		if !pending {
			return false, nil
		}
		target, absolute = max(0, count-1), true
	case "G", "home", "end":
		absolute = true
		if key == "home" {
			target = 0
		} else if count > 0 {
			target = count - 1
		} else {
			target = 1 << 30
		}
	case "j", "down":
		delta = max(1, count)
	case "k", "up":
		delta = -max(1, count)
	case "ctrl+d":
		delta = max(1, m.layout().body/2) * max(1, count)
	case "ctrl+u":
		delta = -max(1, m.layout().body/2) * max(1, count)
	case "pgdown":
		delta = max(1, m.layout().body-1) * max(1, count)
	case "pgup":
		delta = -max(1, m.layout().body-1) * max(1, count)
	case "#":
		m.relativeNumbers = !m.relativeNumbers
		return true, nil
	default:
		return false, nil
	}
	if m.doc != nil && !m.showQueue && (m.focus || key == "pgdown" || key == "pgup") {
		if absolute {
			m.docOffset = target
		}
		m.scrollDocument(delta)
		return true, m.requestDocumentImages()
	}
	if m.showQueue {
		if !absolute {
			target = m.queueCursor + delta
		}
		m.queueCursor = max(0, min(len(m.queue)-1, target))
		return true, nil
	}
	if !absolute {
		target = m.cursor + delta
	}
	m.cursor = max(0, min(len(m.entries)-1, target))
	return true, m.previewSelection()
}
