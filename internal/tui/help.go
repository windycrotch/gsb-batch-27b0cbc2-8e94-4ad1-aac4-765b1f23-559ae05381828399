package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/render"
)

type helpBinding struct{ keys, action string }
type helpSection struct {
	title, shade string
	bindings     []helpBinding
}

func (m *Model) helpSections() []helpSection {
	return []helpSection{
		{"NAVIGATION", accent, []helpBinding{
			{"↑ ↓ / j k", "Select an entry"}, {"Enter", "Open folder or file; play media"},
			{"Backspace / Esc", "Parent folder / back"}, {"gg / G", "First / last entry"},
			{"12G · 5j / 5k", "Jump to line; move by count"}, {"Ctrl+D / Ctrl+U", "Half page down / up"},
			{"PgDn / PgUp", "Next / previous page"}, {"#", "Toggle relative line numbers"},
			{"f / B", "Focus preview / browser only"}, {"H", "Show hidden files"},
		}},
		{"PLAYBACK", "115;185;240", []helpBinding{
			{"Space / p", "Play / pause"}, {"s", "Stop and reset"},
			{"[ / ]", "Seek backward / forward " + m.seekLabel()}, {"{ / }", "Seek backward / forward 30s"},
			{",", "Seek step: 1, 5, 10, 30 or 60s"}, {"t / Tab", "Focus timeline"},
			{"← / →", "Seek on focused timeline"}, {"z / x / c", "Slower / faster / reset speed"},
			{"0", "Seek to start"}, {"Home / End", "Timeline start / end"},
			{"- / = / m", "Volume down / up / mute"}, {"r", "Cycle repeat mode"},
			{"F", "Video FPS: 1, 6, 12, 24, 30, 60"},
			{"D", "Image duration: 2, 5, 10, 30s"},
		}},
		{"SEARCH & SORT", warning, []helpBinding{
			{"/ or Ctrl+F", "Filter files and folders"}, {"rec-* · *.txt", "Glob filters; plain text is fuzzy"},
			{"S", "Open sort menu"}, {"n / m / c", "Sort by name / modified / created"},
			{"s / t / r", "Sort by size / type / reverse"},
		}},
		{"READING", "190;158;235", []helpBinding{
			{"Enter / f", "Focus document"}, {"↑ ↓ / j k", "Scroll focused document"},
			{"Home / End", "Document start / end"}, {"PgUp / PgDn", "Previous / next page"},
			{"/ or Ctrl+F", "Find in focused file"}, {"n / N", "Next / previous match"},
			{"Ctrl+U", "Clear search while typing"}, {"R", "Source / rendered Markdown"},
		}},
		{"FILE ACTIONS", danger, []helpBinding{
			{"e", "Open in default editor"}, {"Ctrl+R", "Rename"}, {"M", "Move"},
			{"d / Delete", "Delete with confirmation"}, {"y", "Confirm deletion, including contents"},
			{"Enter / Esc / n", "Cancel deletion"},
		}},
		{"QUEUE & MOUSE", accent, []helpBinding{
			{"a / Q", "Add to queue / show queue"}, {"n / b", "Next / previous track"},
			{"v", "Show now playing"}, {"d in queue", "Remove queue entry only"},
			{"Click timeline", "Seek to position"}, {"Click transport", "Control now-playing media"},
			{"Scroll wheel", "Scroll document or help"},
		}},
	}
}

func (m *Model) helpContent(w int) []string {
	columns := 1
	if w >= 96 {
		columns = 2
	}
	if w >= 144 {
		columns = 3
	}
	gap, margin := 4, 2
	columnWidth := max(1, (w-2*margin-gap*(columns-1))/columns)
	stacks := make([][]string, columns)
	for i, section := range m.helpSections() {
		col := i % columns
		rows := []string{"\x1b[1m" + color(section.title, section.shade) + "\x1b[22m", color(strings.Repeat("─", columnWidth), border), ""}
		keyWidth := 16
		for _, binding := range section.bindings {
			if columnWidth < 42 {
				rows = append(rows, color(binding.keys, section.shade))
				for _, line := range strings.Split(ansi.Wrap(binding.action, max(1, columnWidth-2), ""), "\n") {
					rows = append(rows, "  "+color(line, fg))
				}
				continue
			}
			key := color(render.Fit(binding.keys, keyWidth), section.shade)
			lines := strings.Split(ansi.Wrap(binding.action, max(1, columnWidth-keyWidth-2), ""), "\n")
			for j, line := range lines {
				prefix := strings.Repeat(" ", keyWidth)
				if j == 0 {
					prefix = key
				}
				rows = append(rows, prefix+"  "+color(line, fg))
			}
		}
		stacks[col] = append(stacks[col], rows...)
		stacks[col] = append(stacks[col], "", "")
	}
	count := 0
	for _, stack := range stacks {
		count = max(count, len(stack))
	}
	rows := []string{""}
	for y := 0; y < count; y++ {
		row := strings.Repeat(" ", margin)
		for col, stack := range stacks {
			line := ""
			if y < len(stack) {
				line = stack[y]
			}
			if col > 0 {
				row += strings.Repeat(" ", gap)
			}
			row += render.Fit(line, columnWidth)
		}
		rows = append(rows, row)
	}
	return rows
}

func (m *Model) scrollHelp(delta int) {
	limit := max(0, len(m.helpContent(m.width-4))-max(1, m.layout().body-1))
	m.helpOffset = max(0, min(limit, m.helpOffset+delta))
}

func (m *Model) helpRows(w, h int) []string {
	content := m.helpContent(w)
	page := max(1, h-1)
	m.helpOffset = max(0, min(m.helpOffset, max(0, len(content)-page)))
	end := min(len(content), m.helpOffset+page)
	rows := append([]string(nil), content[m.helpOffset:end]...)
	for len(rows) < page {
		rows = append(rows, "")
	}
	label := "  ? / Esc Close help"
	if len(content) > page {
		label += fmt.Sprintf("   ·   %d–%d / %d   ·   ↑↓ scroll", m.helpOffset+1, end, len(content))
	}
	return append(rows, color(ansi.Truncate(label, w, ""), muted))
}
