package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/media"
)

func (m *Model) SetVideoFPS(fps int) { m.targetFPS = fps }
func (m *Model) videoFPS() int {
	if m.targetFPS < 1 || m.targetFPS > 60 {
		return int(media.FPS)
	}
	return m.targetFPS
}
func (m *Model) clockedVideo() bool {
	return m.displayingActive && (m.active.Kind == media.Video || m.active.Kind == media.GIF)
}
func (m *Model) frameTolerance() float64 { return max(0.2, 1.0/float64(m.videoFPS())) }
func (m *Model) restartVideoFrames() tea.Cmd {
	// Keep the last picture while the fresh decoder seeks to the audio clock.
	png, rows := m.nativePNG, m.rows
	cmd := m.showActive()
	m.nativePNG, m.rows = png, rows
	return cmd
}
func (m *Model) cycleVideoFPS() tea.Cmd {
	if m.active.Kind != media.Video && m.active.Kind != media.GIF {
		return nil
	}
	current, next := m.videoFPS(), 1
	for _, rate := range []int{1, 6, 12, 24, 30, 60} {
		if rate > current {
			next = rate
			break
		}
	}
	m.targetFPS = next
	m.notice = fmt.Sprintf("Video target %d FPS · F changes rate", next)
	if m.displayingActive {
		return m.restartVideoFrames()
	}
	return nil
}
func recentFrames(times []time.Time, now time.Time) []time.Time {
	first := 0
	for first < len(times) && now.Sub(times[first]) >= time.Second {
		first++
	}
	return times[first:]
}
func (m *Model) videoFPSLabel() string {
	if m.active.Kind != media.Video && m.active.Kind != media.GIF {
		return ""
	}
	if !m.displayingActive || m.state != "Playing" {
		return fmt.Sprintf(" · %d FPS target · F rate", m.videoFPS())
	}
	if m.graphics != nil {
		return fmt.Sprintf(" · %d/%d FPS sent/target · F rate", m.graphics.FrameRate(), m.videoFPS())
	}
	return fmt.Sprintf(" · %d/%d FPS rendered/target · F rate", len(recentFrames(m.frameTimes, time.Now())), m.videoFPS())
}
