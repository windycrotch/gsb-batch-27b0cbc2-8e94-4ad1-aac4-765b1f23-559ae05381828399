package render

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/lancekrogers/peep/internal/media"
	"strings"
	"testing"
)

func TestCells(t *testing.T) {
	rows := Cells(media.Frame{Width: 1, Height: 2, RGB: []byte{255, 0, 0, 0, 0, 255}})
	if len(rows) != 1 || !strings.Contains(rows[0], "38;2;255;0;0") || !strings.Contains(rows[0], "48;2;0;0;255") || ansi.StringWidth(rows[0]) != 1 {
		t.Fatalf("bad cell: %q", rows)
	}
}
func TestSafeAndFit(t *testing.T) {
	s := Safe("file\x1b[2J\n\t\u202ename")
	if strings.ContainsAny(s, "\x1b\n\t\u202e") {
		t.Fatal("terminal controls survived")
	}
	if ansi.StringWidth(Fit("界abcdef", 4)) != 4 {
		t.Fatal("wrong display width")
	}
}
