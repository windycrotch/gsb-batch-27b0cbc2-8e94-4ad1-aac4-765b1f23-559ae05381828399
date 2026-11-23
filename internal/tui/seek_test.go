package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAdjustableSeek(t *testing.T) {
	m := imageModel()
	m.duration = 120
	m.position = 10
	m.state = "Paused"
	m.SetSeekStep(500 * time.Millisecond)
	key := func(s string) { m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	key("]")
	if m.position != 10.5 || m.state != "Paused" {
		t.Fatal("fractional seek failed")
	}
	key("[")
	if m.position != 10 {
		t.Fatal("backward seek failed")
	}
	if !strings.Contains(m.buttons()[0].label, "-0.5s") {
		t.Fatal("button label does not match jump")
	}
	key(",")
	key("]")
	if m.position != 11 || m.seekSeconds() != 1 {
		t.Fatal("step cycling failed")
	}
	m.timeline = true
	m.key(tea.KeyMsg{Type: tea.KeyRight})
	if m.position != 12 {
		t.Fatal("timeline ignored selected step")
	}
	m.SetSeekStep(time.Minute)
	key(",")
	if m.seekSeconds() != 1 {
		t.Fatal("cycle did not wrap")
	}
	m.SetSeekStep(-time.Second)
	if m.seekSeconds() != 1 {
		t.Fatal("invalid step replaced valid one")
	}
	m.position = 119.5
	key("]")
	if m.position != 120 {
		t.Fatal("seek crossed duration")
	}
	m.position = .2
	key("[")
	if m.position != 0 {
		t.Fatal("seek crossed start")
	}
	m.filtering = true
	key(",")
	if m.filter != "," || m.seekSeconds() != 1 {
		t.Fatal("step shortcut intercepted search")
	}
}

func TestPreviewLabelsCellFallback(t *testing.T) {
	m := imageModel()
	m.preview = m.active
	m.preview.HasVisual = true
	m.focus = true
	if !strings.Contains(m.View(), "Preview · cells") {
		t.Fatal("cell fallback not identified")
	}
}
