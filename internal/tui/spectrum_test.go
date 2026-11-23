package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/media"
)

func TestSpectrumAxisAndNarrowTreble(t *testing.T) {
	for _, width := range []int{13, 24, 47, 95} {
		axis := spectrumAxis(width)
		if len(axis) != width || !strings.HasPrefix(axis, "20 Hz") || !strings.HasSuffix(axis, "20 kHz") {
			t.Fatalf("bad %d-column axis: %q", width, axis)
		}
	}
	axis := spectrumAxis(95)
	if !strings.Contains(axis, "100") || !strings.Contains(axis, "1k") {
		t.Fatal("missing interior ticks")
	}
	m := imageModel()
	m.volume = 100
	m.state = "Playing"
	m.bands[media.SpectrumBands-1] = 1
	for _, width := range []int{1, 12, 30, 100} {
		rows := m.spectrumRows(width, 20)
		found := false
		for _, row := range rows {
			if strings.Contains(row, "█") {
				found = true
			}
		}
		if !found {
			t.Fatalf("high treble vanished at width %d", width)
		}
		// The chart rows (excluding metadata) must fit even very small previews.
		for _, row := range rows {
			if strings.Contains(row, "░") || strings.Contains(row, "█") {
				if ansi.StringWidth(row) > width {
					t.Fatalf("chart exceeds %d columns", width)
				}
			}
		}
	}
}
