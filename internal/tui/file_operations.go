package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/fileops"
	"github.com/lancekrogers/peep/internal/render"
)

type filePrompt struct {
	operation, input, problem string
	target                    fileops.Target
	working                   bool
}
type fileOperationMsg struct {
	from, to, operation string
	err                 error
}
type editorDoneMsg struct{ err error }

func (m *Model) beginFileOperation(operation string) tea.Cmd {
	if m.busy {
		m.notice = "Wait for playback to finish loading or seeking"
		return nil
	}
	if (m.focus && operation != "editor") || m.showQueue || m.timeline {
		m.notice = "Return to the file browser to modify a file"
		return nil
	}
	e := m.selected()
	if e == nil || e.Name == ".." {
		m.notice = "Select a file or folder, not the parent entry"
		return nil
	}
	if operation == "editor" {
		cmd, err := fileops.Editor(m.ctx, e.Path)
		if err != nil {
			m.notice = err.Error()
			return nil
		}
		// Editors need real terminal descriptors. Passing the graphics/probe
		// wrappers makes os/exec create pipes (and a stdin copy goroutine that
		// can remain blocked after the editor exits).
		input := m.terminalInput
		if input == nil {
			input = os.Stdin
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = input, os.Stdout, os.Stderr
		m.editing = true
		m.syncGraphics()
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return editorDoneMsg{err} })
	}
	if m.active.Path != "" && m.state != "Empty" && m.state != "Ended" && m.state != "Stopped" {
		relative, err := filepath.Rel(e.Path, m.active.Path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			m.notice = "Stop playback before modifying its file or containing folder"
			return nil
		}
	}
	target, err := fileops.Capture(e.Path)
	if err != nil {
		m.notice = err.Error()
		return nil
	}
	m.filePrompt = &filePrompt{operation: operation, target: target}
	if operation == "rename" {
		m.filePrompt.input = filepath.Base(e.Path)
	}
	return nil
}
func (m *Model) fileOperationKey(k tea.KeyMsg) tea.Cmd {
	p := m.filePrompt
	if p.working {
		return nil
	}
	key := k.String()
	if p.operation == "delete" {
		switch key {
		case "y", "Y":
			key = "enter"
		case "n", "N", "esc", "enter":
			m.filePrompt = nil
			return m.requestDocumentImages()
		default:
			return nil
		}
	}
	switch key {
	case "esc":
		m.filePrompt = nil
		return m.requestDocumentImages()
	case "ctrl+u":
		p.input = ""
	case "backspace":
		r := []rune(p.input)
		if len(r) > 0 {
			p.input = string(r[:len(r)-1])
		}
	case "enter":
		destination := ""
		if p.operation != "delete" {
			var err error
			destination, err = fileops.Destination(p.target, p.input, p.operation == "rename")
			if err != nil {
				p.problem = err.Error()
				return nil
			}
		}
		p.working = true
		target, operation, ctx := p.target, p.operation, m.ctx
		return func() tea.Msg {
			var err error
			if operation == "delete" {
				err = fileops.Delete(ctx, target)
			} else {
				err = fileops.Move(ctx, target, destination)
			}
			return fileOperationMsg{target.Path, destination, operation, err}
		}
	default:
		if len(k.Runes) > 0 && len(p.input)+len(string(k.Runes)) <= 4096 {
			p.input += string(k.Runes)
		}
	}
	p.problem = ""
	return nil
}
func (m *Model) fileOperationRows(w, h int) []string {
	p := m.filePrompt
	if p.operation == "delete" {
		return m.deleteRows(w, h)
	}
	heading := displayPath(p.target.Path)
	detail := "New name; existing destinations are never overwritten."
	if p.operation == "move" {
		detail = "Destination folder/path; same filesystem; no overwrites."
	}

	var wrapped []string
	for _, row := range []string{heading, "", detail} {
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(row, max(1, w), true), "\n")...)
	}
	// Keep input, errors and cancellation visible in short terminals.
	if len(wrapped) > max(1, h-4) {
		wrapped = wrapped[:max(1, h-4)]
	}
	problem := render.Safe(p.problem)
	if p.working {
		problem = "Working…"
	}

	wrapped = append(wrapped, color("> "+render.Safe(p.input)+"▏", fg), color(problem, accent), "Enter confirm · Esc cancel · Ctrl+U clear")
	return wrapped
}
func (m *Model) applyFileOperation(msg fileOperationMsg) tea.Cmd {
	if msg.err != nil {
		if m.filePrompt != nil {
			m.filePrompt.working = false
			m.filePrompt.problem = msg.err.Error()
		}
		return nil
	}
	m.filePrompt = nil
	// Update queued and stopped media paths, including descendants of a moved folder.
	remap := func(path string) string {
		rel, err := filepath.Rel(msg.from, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return path
		}
		if msg.to == "" {
			return ""
		}
		return filepath.Join(msg.to, rel)
	}
	next := m.queue[:0]
	newIndex := -1
	for i, path := range m.queue {
		path = remap(path)
		if path != "" {
			if i == m.index {
				newIndex = len(next)
			}
			next = append(next, path)
		}
	}
	m.queue = next
	results := m.resultPaths[:0]
	for _, path := range m.resultPaths {
		if path = remap(path); path != "" {
			results = append(results, path)
		}
	}
	m.resultPaths = results
	m.resultSelected = remap(m.resultSelected)
	m.index = newIndex
	m.queueCursor = max(0, min(m.queueCursor, len(next)-1))
	m.active.Path = remap(m.active.Path)
	if m.active.Path == "" {
		m.state = "Empty"
		m.position = 0
		m.duration = 0
	}
	m.wanted = msg.to
	m.filter = ""
	m.notice = fmt.Sprintf("%s complete", msg.operation)
	return m.readDir()
}
func (m *Model) editorFinished(msg editorDoneMsg) tea.Cmd {
	m.editing = false
	m.last = time.Now()
	if msg.err != nil {
		m.notice = "Editor: " + msg.err.Error()
	} else {
		m.notice = "Editor closed; refreshed files"
	}
	if e := m.selected(); e != nil {
		m.wanted = e.Path
	}
	return m.readDir()
}
