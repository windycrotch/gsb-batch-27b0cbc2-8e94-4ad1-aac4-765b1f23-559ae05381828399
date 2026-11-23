package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestHomePathDisplay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for path, want := range map[string]string{
		home: "~", filepath.Join(home, "images", "photo.png"): "~/images/photo.png",
		home + "-other/photo.png": home + "-other/photo.png", "/tmp/photo.png": "/tmp/photo.png",
	} {
		if got := displayPath(path); got != want {
			t.Errorf("displayPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHelpResponsiveScrolling(t *testing.T) {
	for _, width := range []int{35, 80, 110, 180} {
		m := imageModel()
		m.width, m.height, m.help = width, 24, true
		content := strings.Join(m.helpContent(width-4), "\n")
		for _, section := range m.helpSections() {
			if !strings.Contains(ansi.Strip(content), section.title) {
				t.Fatal("missing section", section.title)
			}
			for _, binding := range section.bindings {
				if !strings.Contains(ansi.Strip(content), binding.keys) {
					t.Fatal("truncated shortcut", width, binding.keys)
				}
			}
		}
		cursor := m.cursor
		m.key(tea.KeyMsg{Type: tea.KeyDown})
		if m.helpOffset == 0 || m.cursor != cursor {
			t.Fatal("help navigation changed browser selection or did not scroll")
		}
		m.key(tea.KeyMsg{Type: tea.KeyEnd})
		last := m.helpOffset
		m.mouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
		if m.helpOffset >= last {
			t.Fatal("help wheel did not scroll")
		}
		for _, row := range m.helpRows(width-4, m.layout().body) {
			if ansi.StringWidth(row) > width-4 {
				t.Fatal("help row overflows", width, ansi.StringWidth(row))
			}
		}
		m.width, m.height = 180, 80
		m.helpRows(176, m.layout().body)
		if m.helpOffset != 0 {
			t.Fatal("resize left help scrolled past content")
		}
		m.key(tea.KeyMsg{Type: tea.KeyEsc})
		if m.help {
			t.Fatal("escape did not close help")
		}
	}
}
