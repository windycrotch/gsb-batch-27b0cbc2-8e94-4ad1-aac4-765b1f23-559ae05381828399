package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/media"
	"github.com/lancekrogers/peep/internal/player"
)

// NewResults starts a browser restricted to the supplied paths, in input order.
func NewResults(ctx context.Context, p *player.Player, paths []string, play bool, notice string) (*Model, error) {
	m, err := New(ctx, p, nil, false, notice)
	if err != nil {
		return nil, err
	}
	m.resultsMode, m.resultBase = true, m.dir
	seen := make(map[string]bool)
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(abs); err != nil {
			return nil, fmt.Errorf("stdin path %q: %w (pipe filenames with rg -l, not matching lines)", path, err)
		}
		if play {
			info, err := os.Stat(abs)
			if err != nil || !info.Mode().IsRegular() || media.Classify(abs) == "" {
				return nil, fmt.Errorf("not playable media: %q (omit --play to preview)", path)
			}
		}
		if !seen[abs] {
			m.resultPaths = append(m.resultPaths, abs)
			seen[abs] = true
		}
	}
	if play && len(m.resultPaths) > 0 {
		m.queue = append([]string(nil), m.resultPaths...)
		m.index = -2
	}
	return m, nil
}

// SetTerminalInput also gives external editors a real TTY when stdin was a pipe.
func (m *Model) SetTerminalInput(input *os.File) { m.terminalInput = input }

func (m *Model) parent() tea.Cmd {
	if m.resultsMode {
		return nil
	}
	if m.resultRoot != "" && m.dir == m.resultRoot {
		m.resultsMode = true
		m.resultRoot = ""
		m.dir = m.resultBase
		m.focus, m.showQueue, m.timeline = false, false, false
		m.filter = ""
		m.wanted = m.resultSelected
		return m.readDir()
	}
	return m.navigate(filepath.Dir(m.dir))
}

func (m *Model) parentHint() string {
	if m.resultsMode {
		return "stdin results"
	}
	if m.resultRoot != "" && m.dir == m.resultRoot {
		return "‹ Backspace results"
	}
	return "‹ Backspace parent"
}
