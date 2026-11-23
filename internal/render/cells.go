// Package render converts decoded pixels into ANSI true-color terminal cells.
package render

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/media"
)

func Cells(f media.Frame) []string {
	if len(f.RGB) != f.Width*f.Height*3 {
		return nil
	}
	rows := make([]string, 0, f.Height/2)
	for y := 0; y+1 < f.Height; y += 2 {
		var b strings.Builder
		for x := 0; x < f.Width; x++ {
			top := (y*f.Width + x) * 3
			bottom := ((y+1)*f.Width + x) * 3
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", f.RGB[top], f.RGB[top+1], f.RGB[top+2], f.RGB[bottom], f.RGB[bottom+1], f.RGB[bottom+2])
		}
		b.WriteString("\x1b[0m")
		rows = append(rows, b.String())
	}
	return rows
}

// Safe prevents filenames and metadata from injecting terminal control sequences.
func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return '�'
		}
		return r
	}, s)
}
func Fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}
