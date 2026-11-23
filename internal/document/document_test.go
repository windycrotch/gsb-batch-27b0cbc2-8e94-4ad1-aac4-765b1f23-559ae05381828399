package document

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
)

func TestReadBoundsAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.md")
	os.WriteFile(path, []byte(strings.Repeat("a", MaxBytes-1)+"🎵tail"), 0600)
	d, err := Read(context.Background(), path)
	if err != nil || !d.Truncated {
		t.Fatalf("truncation: %v", err)
	}
	os.WriteFile(path, []byte("hello\x00binary"), 0600)
	if _, err = Read(context.Background(), path); err == nil {
		t.Fatal("binary accepted")
	}
	fifo := filepath.Join(dir, "fifo.md")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err = Read(context.Background(), fifo); err == nil {
		t.Fatal("FIFO accepted")
	}
	if time.Since(before) > time.Second {
		t.Fatal("FIFO blocked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Read(ctx, path); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestMarkdownLayout(t *testing.T) {
	d := &Document{Kind: "MD", Source: "# A heading\n\nA **bold** paragraph with ![example][pic].\n\n| One | Two |\n|---|---|\n| a | b |\n\n```mermaid\nflowchart LR\n A --> B\n```\n\n[pic]: ./an%20image.png\n"}
	l, err := Render(context.Background(), d, 42, false)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, line := range l.Lines {
		if ansi.StringWidth(line.Text) > 42 {
			t.Fatal("overflow")
		}
		text.WriteString(ansi.Strip(line.Text))
	}
	if !strings.Contains(text.String(), "A heading") || !strings.Contains(text.String(), "bold") || !strings.Contains(text.String(), "One") {
		t.Fatal(text.String())
	}
	if len(l.Assets) != 2 || l.Assets[0].Ref != "./an%20image.png" || l.Assets[1].Kind != "mermaid" {
		t.Fatalf("assets: %+v", l.Assets)
	}
	raw, err := Render(context.Background(), d, 42, true)
	if err != nil || !strings.Contains(raw.Lines[0].Text, "# A heading") {
		t.Fatal("source mode lost Markdown")
	}
}
func TestEmptyAndUntrustedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	os.WriteFile(path, []byte("# Title\n\n\x1b]52;c;payload\a\n\n&#27;]52;c;other&#7;\n\n<script>alert(1)</script>"), 0600)
	d, err := Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Render(context.Background(), d, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range l.Lines {
		if strings.Contains(line.Text, "\x1b]") || strings.ContainsRune(line.Text, '\a') {
			t.Fatal("document emitted terminal commands")
		}
	}
	for _, raw := range []bool{false, true} {
		l, e := Render(context.Background(), &Document{Kind: "MD"}, 20, raw)
		if e != nil || len(l.Lines) == 0 {
			t.Fatal("empty document has no viewport")
		}
	}
}
