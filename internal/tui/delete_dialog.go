package tui

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/render"
)

// displayPath changes presentation only; filesystem operations keep absolute paths.
func displayPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		if path == home {
			path = "~"
		} else if home != string(filepath.Separator) && strings.HasPrefix(path, home+string(filepath.Separator)) {
			path = "~" + strings.TrimPrefix(path, home)
		}
	}
	return render.Safe(path)
}

func (m *Model) deleteTitle() string {
	if m.filePrompt.target.Info.IsDir() {
		return "Delete folder?"
	}
	if m.filePrompt.target.Info.Mode()&os.ModeSymlink != 0 {
		return "Delete symlink?"
	}
	return "Delete file?"
}

func (m *Model) deleteRows(w, h int) []string {
	p := m.filePrompt
	pad := 2
	if w < 40 {
		pad = 1
	}
	inner := max(1, w-2*pad)
	wrap := func(text, shade string) []string {
		lines := strings.Split(ansi.Wrap(text, inner, ""), "\n")
		for i := range lines {
			lines[i] = color(lines[i], shade)
		}
		return lines
	}
	name := "\x1b[1m" + color(ansi.Truncate(render.Safe(filepath.Base(p.target.Path)), inner, "…"), fg) + "\x1b[22m"
	path := color(ansi.Truncate(displayPath(p.target.Path), inner, "…"), muted)
	warn := "This file will be permanently deleted."
	if p.target.Info.IsDir() {
		warn = "All files and subdirectories inside will also be deleted."
	} else if p.target.Info.Mode()&os.ModeSymlink != 0 {
		warn = "Only this link will be deleted. Its target stays untouched."
	}
	body := wrap(warn, warning)
	body = append(body, wrap("This cannot be undone.", muted)...)
	if p.problem != "" {
		body = append(body, wrap(render.Safe(p.problem), danger)...)
	}
	button := func(label, background string) string {
		return "\x1b[48;2;" + background + "m" + color(" "+label+" ", fg) + "\x1b[48;2;" + panelBG + "m"
	}
	buttons := button("Enter  Cancel", selectedBG) + "  " + button("y  Delete", "93;43;54")
	if inner < 28 {
		buttons = color("Enter Cancel", accent) + "  " + color("y Delete", danger)
	}
	footer := []string{buttons, color("Esc / n also cancel", muted)}
	if p.working {
		footer = []string{color("Deleting…", warning)}
	}
	rows := []string{"", name, path, ""}
	rows = append(rows, body...)
	rows = append(rows, "")
	rows = append(rows, footer...)
	rows = append(rows, "")
	if len(rows) > h {
		// Compact terminals retain the warning and both decisions first.
		rows = append([]string{name}, body...)
		if len(rows)+len(footer) < h {
			rows = append([]string{name, path}, body...)
		}
		available := max(1, h-len(footer))
		if len(rows) > available {
			rows = rows[:available]
		}
		rows = append(rows, footer...)
	}
	for i := range rows {
		rows[i] = strings.Repeat(" ", pad) + rows[i]
	}
	return rows
}
