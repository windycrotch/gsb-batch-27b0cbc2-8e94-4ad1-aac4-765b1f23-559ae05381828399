package document

import (
	"context"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestSyntaxLanguages(t *testing.T) {
	for path, source := range map[string]string{"main.go": "package main\nfunc main() {}", "lib.rs": "fn main() { let x = 12; }", "main.py": "def greet():\n    return 'hello'", "app.ts": "const count: number = 42;", "config.yaml": "name: peep\nenabled: true", "config.toml": "name = \"peep\"", "config.json": "{\"name\":\"peep\"}", "run.sh": "#!/bin/sh\necho \"hello\"", "Dockerfile": "FROM alpine\nRUN echo hello"} {
		t.Run(path, func(t *testing.T) {
			d := &Document{Path: path, Kind: Kind(path), Source: source}
			l, err := Render(context.Background(), d, 80, false)
			if err != nil {
				t.Fatal(err)
			}
			var rows []string
			for _, r := range l.Lines {
				rows = append(rows, r.Text)
			}
			got := strings.Join(rows, "\n")
			if !strings.Contains(got, "\x1b[38;2;") {
				t.Fatal("no syntax color", got)
			}
			if ansi.Strip(got) != source {
				t.Fatalf("source changed: %q", ansi.Strip(got))
			}
		})
	}
}
func TestSyntaxFallbackAndFences(t *testing.T) {
	source := "unknown plain text\nsecond line"
	if got := highlight(context.Background(), source, "notes.unknown", ""); got != source {
		t.Fatal(got)
	}
	big := strings.Repeat("x", MaxHighlightBytes+1)
	if highlight(context.Background(), big, "main.go", "") != big {
		t.Fatal("large fallback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Render(ctx, &Document{Path: "main.go", Source: "package main"}, 30, false); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, src := range []string{"```go\npackage main\nfunc main() {}\n```", "> ```python\n> def greet():\n>     return 42\n> ```"} {
		l, err := Render(context.Background(), &Document{Kind: "MD", Source: src}, 40, false)
		if err != nil {
			t.Fatal(err)
		}
		var joined string
		for _, r := range l.Lines {
			joined += r.Text
			if ansi.StringWidth(r.Text) > 40 {
				t.Fatal("wide fence")
			}
		}
		if !strings.Contains(joined, "\x1b[38;2;255;121;198m") {
			t.Fatal("fence not highlighted", joined)
		}
	}
}

func TestNestedFenceOrder(t *testing.T) {
	d := &Document{Kind: "MD", Source: "> Before fence\n>\n> ```go\n> package main\n> ```\n>\n> After fence\n"}
	l, err := Render(context.Background(), d, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, line := range l.Lines {
		text += ansi.Strip(line.Text) + "\n"
	}
	if !(strings.Index(text, "Before fence") < strings.Index(text, "package main") && strings.Index(text, "package main") < strings.Index(text, "After fence")) {
		t.Fatal("fence reordered content", text)
	}
}
