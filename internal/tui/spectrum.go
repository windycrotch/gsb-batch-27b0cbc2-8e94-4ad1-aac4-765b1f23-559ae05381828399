package tui

import (
	"context"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/media"
	"github.com/lancekrogers/peep/internal/render"
	"math"
	"path/filepath"
	"strings"
)

type spectrumMsg struct {
	id    int
	frame media.SpectrumFrame
	ok    bool
}

func (m *Model) clearSpectrum() {
	if m.spectrumCancel != nil {
		m.spectrumCancel()
	}
	m.spectrumID++
	m.spectrum = nil
	m.spectrumPending = nil
	m.bands = [media.SpectrumBands]float64{}
	m.bandTarget = m.bands
	m.spectrumError = ""
}
func (m *Model) startSpectrum() tea.Cmd {
	m.clearSpectrum()
	if m.active.Kind != media.Audio || m.state == "Stopped" || m.state == "Error" {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.spectrumCancel = cancel
	m.spectrum = media.Spectrum(ctx, m.active.Path, m.position)
	return m.nextSpectrum()
}
func (m *Model) nextSpectrum() tea.Cmd {
	ch, id := m.spectrum, m.spectrumID
	if ch == nil {
		return nil
	}
	return func() tea.Msg { f, ok := <-ch; return spectrumMsg{id, f, ok} }
}
func (m *Model) spectrumRows(w, h int) []string {
	out := make([]string, h)
	if h < 7 {
		return out
	}
	title := m.active.Title
	if title == "" {
		title = filepath.Base(m.active.Path)
	}
	barHeight := max(1, min(12, h-6))
	top := max(0, (h-barHeight-6)/2)
	out[top] = center(color("♪  "+render.Safe(title), accent), w)
	out[top+1] = center(color(render.Safe(m.active.Artist), muted), w)
	columns := max(1, min(media.SpectrumBands, w/3))
	barWidth := max(1, min(3, (w-columns)/columns))
	total := columns*(barWidth+1) - 1
	left := max(0, (w-total)/2)
	for row := 0; row < barHeight; row++ {
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", left))
		for col := 0; col < columns; col++ {
			// Aggregate all bands at narrow widths; do not drop high treble.
			level := 0.0
			for band := col * media.SpectrumBands / columns; band < (col+1)*media.SpectrumBands/columns; band++ {
				level = max(level, m.bands[band])
			}
			if m.muted {
				level = 0
			} else {
				level *= m.volume / 100
			}
			lit := int(math.Ceil(level*float64(barHeight))) > barHeight-row-1
			symbol := "░"
			shade := border
			if lit {
				symbol = "█"
				shade = accent
				if row < barHeight/3 {
					shade = "245;181;98"
				} else if row < 2*barHeight/3 {
					shade = "98;190;242"
				}
			}
			b.WriteString(color(strings.Repeat(symbol, barWidth), shade))
			if col+1 < columns {
				b.WriteByte(' ')
			}
		}
		out[top+3+row] = b.String()
	}
	out[top+barHeight+3] = strings.Repeat(" ", left) + color(spectrumAxis(total), muted)
	label := "AUDIO SPECTRUM · nominal hearing range"
	if m.state == "Paused" {
		label = "PAUSED · spectrum frozen"
	}
	if m.state == "Stopped" || m.state == "Ended" {
		label = fmt.Sprintf("%s · Space to play", m.state)
	}
	if m.spectrumError != "" {
		label = "Spectrum unavailable · audio playback continues"
	}
	if top+barHeight+4 < h {
		out[top+barHeight+4] = center(color(label, muted), w)
	}
	return out
}

// Labels sit on the same logarithmic axis as the bars. Keep endpoints at small
// widths and add interior ticks only when they fit without touching.
func spectrumAxis(width int) string {
	if width < 13 {
		return ""
	}
	axis := []byte(strings.Repeat(" ", width))
	occupied := make([]bool, width)
	place := func(label string, at int) {
		at = max(0, min(width-len(label), at))
		for i := max(0, at-1); i < min(width, at+len(label)+1); i++ {
			if occupied[i] {
				return
			}
		}
		copy(axis[at:], label)
		for i := at; i < at+len(label); i++ {
			occupied[i] = true
		}
	}
	place("20 Hz", 0)
	place("20 kHz", width-6)
	for _, tick := range []struct {
		hz    float64
		label string
	}{{100, "100"}, {1000, "1k"}, {10000, "10k"}} {
		at := int(math.Round(math.Log(tick.hz/media.SpectrumMinHz) / math.Log(media.SpectrumMaxHz/media.SpectrumMinHz) * float64(width-1)))
		place(tick.label, at-len(tick.label)/2)
	}
	return string(axis)
}
