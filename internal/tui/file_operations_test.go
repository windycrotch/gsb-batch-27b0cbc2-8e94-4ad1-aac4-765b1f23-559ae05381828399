package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/browser"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryDeleteConfirmation(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "recordings")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "clip.txt")
	if err := os.WriteFile(child, []byte("keep until confirmed"), 0600); err != nil {
		t.Fatal(err)
	}
	m := imageModel()
	m.dir = parent
	m.entries = []browser.Entry{{Name: "recordings", Path: dir, Dir: true}}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune("n")}} {
		m.beginFileOperation("delete")
		rows := strings.Join(m.fileOperationRows(100, 20), "\n")
		if !strings.Contains(rows, "recordings") || !strings.Contains(rows, "All files and subdirectories") {
			t.Fatal("missing recursive deletion warning", rows)
		}
		if !strings.Contains(strings.Join(m.fileOperationRows(60, 8), "\n"), "Delete") {
			t.Fatal("short prompt lost recursive confirmation label")
		}
		m.key(key)
		if m.filePrompt != nil {
			t.Fatal("cancel did not close prompt")
		}
		if _, err := os.Stat(child); err != nil {
			t.Fatal("cancel deleted contents", err)
		}
	}
	m.beginFileOperation("delete")
	cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("confirmation did not start deletion")
	}
	msg := cmd().(fileOperationMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.queue = []string{child, filepath.Join(parent, "keep.mp3")}
	m.applyFileOperation(msg)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("confirmed directory retained")
	}
	if len(m.queue) != 1 || m.queue[0] != filepath.Join(parent, "keep.mp3") {
		t.Fatal("deleted descendants retained in queue", m.queue)
	}
}

func TestFileConfirmationAndCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quit.txt")
	os.WriteFile(path, []byte("keep"), 0600)
	m := imageModel()
	m.dir = dir
	m.entries = []browser.Entry{{Name: "quit.txt", Path: path}}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune("n")}, {Type: tea.KeyRunes, Runes: []rune("N")}} {
		m.beginFileOperation("delete")
		m.key(key)
		if m.filePrompt != nil {
			t.Fatal("cancel did not close prompt")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("cancel deleted file")
		}
	}
	m.beginFileOperation("delete")
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.filePrompt == nil || m.filePrompt.working || m.filePrompt.input != "" {
		t.Fatal("unrelated key changed confirmation")
	}
	cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("confirmed delete did nothing")
	}
	msg := cmd().(fileOperationMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.applyFileOperation(msg)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file retained")
	}
	m.entries = []browser.Entry{{Name: "..", Path: dir, Dir: true}}
	m.beginFileOperation("delete")
	if m.filePrompt != nil {
		t.Fatal("parent can be deleted")
	}
}

func TestMoveUpdatesQueuePaths(t *testing.T) {
	m := imageModel()
	m.queue = []string{"/folder/one.mp3", "/folder/two.mp3", "/elsewhere/three.mp3"}
	m.active.Path = "/folder/one.mp3"
	m.state = "Stopped"
	m.index = 0
	m.applyFileOperation(fileOperationMsg{from: "/folder", to: "/renamed", operation: "move"})
	if m.active.Path != "/renamed/one.mp3" || m.queue[1] != "/renamed/two.mp3" || m.queue[2] != "/elsewhere/three.mp3" {
		t.Fatal(m.queue, m.active.Path)
	}
	m.applyFileOperation(fileOperationMsg{from: "/renamed/one.mp3", operation: "delete"})
	if len(m.queue) != 2 || m.active.Path != "" || m.state != "Empty" || m.index != -1 {
		t.Fatal(m.queue, m.state, m.index)
	}
}
