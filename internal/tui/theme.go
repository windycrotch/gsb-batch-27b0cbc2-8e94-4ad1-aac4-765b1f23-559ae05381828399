package tui

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/render"
	"strings"
)

const (
	bg         = "17;22;34"
	panelBG    = "23;30;45"
	fg         = "218;226;239"
	muted      = "126;146;170"
	accent     = "88;214;190"
	danger     = "245;135;145"
	warning    = "235;190;105"
	border     = "56;75;98"
	selectedBG = "38;75;89"
)

func color(s, c string) string { return "\x1b[38;2;" + c + "m" + s + "\x1b[39m" }
func fill(s string, w int, background string) string {
	base := "\x1b[38;2;" + fg + "m\x1b[48;2;" + background + "m"
	s = strings.ReplaceAll(render.Fit(s, w), "\x1b[0m", base)
	return base + s + "\x1b[0m"
}
func center(s string, w int) string {
	return strings.Repeat(" ", max(0, (w-ansi.StringWidth(s))/2)) + s
}

type layout struct{ files, preview, queue, panelHeight, body int }

func (m *Model) transportVisible() bool {
	if e := m.selected(); e != nil && !e.Dir && e.Kind != "" {
		return true
	}
	return m.preview.Kind != "" || (m.active.Path != "" && (m.state == "Playing" || m.state == "Paused" || m.state == "Loading"))
}

func (m *Model) layout() layout {
	footer := 3
	if m.transportVisible() {
		footer += 4
	}
	l := layout{panelHeight: max(3, m.height-2-footer)}
	l.body = l.panelHeight - 2
	available := max(4, m.width-2)
	if m.focus {
		l.preview = available
		return l
	}
	if m.width < 70 {
		if m.showQueue {
			l.queue = available
		} else {
			l.files = available
		}
		return l
	}
	if !m.showQueue && (m.browserOnly || m.filtering || m.preview.Path == "") {
		l.files = available
		return l
	}
	l.files = max(24, min(52, m.width/3))
	l.preview = available - l.files - 1
	if m.showQueue {
		if m.width >= 110 {
			l.queue = 26
			l.preview -= 27
		} else {
			l.queue = l.files
			l.files = 0
		}
	}
	return l
}
func box(title string, rows []string, w, h int, focused bool) []string {
	c := border
	if focused {
		c = accent
	}
	return boxColor(title, rows, w, h, c)
}

func boxColor(title string, rows []string, w, h int, c string) []string {
	inner := max(1, w-2)
	title = " " + title + " "
	title = ansi.Truncate(title, inner, "")
	out := []string{fill(color("╭", c)+color(title, fg)+color(strings.Repeat("─", max(0, inner-ansi.StringWidth(title)))+"╮", c), w, bg)}
	for i := 0; i < h-2; i++ {
		s := ""
		if i < len(rows) {
			s = rows[i]
		}
		out = append(out, color("│", c)+fill(s, inner, panelBG)+color("│", c))
	}
	out = append(out, fill(color("╰"+strings.Repeat("─", inner)+"╯", c), w, bg))
	return out
}
func badge(s string) string { return color(fmt.Sprintf(" %s ", s), accent) }
