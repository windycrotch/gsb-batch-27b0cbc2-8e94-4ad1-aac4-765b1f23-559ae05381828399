package document

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMermaidHelperAndCache(t *testing.T) {
	dir := t.TempDir()
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 32, 16)))
	os.WriteFile(filepath.Join(dir, "image.png"), pngData.Bytes(), 0600)
	t.Setenv("PEEP_TEST_PNG", filepath.Join(dir, "image.png"))
	t.Setenv("PEEP_TEST_CALLS", filepath.Join(dir, "calls"))
	helper := filepath.Join(dir, "mmdg")
	os.WriteFile(helper, []byte("#!/bin/sh\ncat >/dev/null\nprintf x >>\"$PEEP_TEST_CALLS\"\ncat \"$PEEP_TEST_PNG\"\n"), 0700)
	r := NewMermaid(helper)
	first, err := r.Render(context.Background(), "flowchart LR\n A-->B", 40)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Render(context.Background(), "flowchart LR\n A-->B", 20)
	if err != nil || !bytes.Equal(first.PNG, second.PNG) || second.Columns != 20 {
		t.Fatal("cache/reflow failed", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "x" {
		t.Fatalf("cached diagram rendered %d times", len(calls))
	}
	for i := 0; i < 18; i++ {
		_, err = r.Render(context.Background(), strings.Repeat("A", i+1), 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(r.cache) > 16 || r.bytes > 32<<20 {
		t.Fatal("unbounded cache")
	}
}

func TestMermaidFailures(t *testing.T) {
	dir := t.TempDir()
	r := NewMermaid(filepath.Join(dir, "missing"))
	if _, err := r.Render(context.Background(), "flowchart LR", 40); err == nil || !strings.Contains(err.Error(), "rendering worker unavailable") {
		t.Fatal(err)
	}
	if _, err := r.Render(context.Background(), strings.Repeat("a", 128<<10+1), 40); err == nil || !strings.Contains(err.Error(), "128 KiB") {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "mmdg")
	os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'invalid PNG'\n"), 0700)
	r = NewMermaid(helper)
	if _, err := r.Render(context.Background(), "graph LR", 40); err == nil {
		t.Fatal("invalid output accepted")
	}
	os.WriteFile(helper, []byte("#!/bin/sh\nexec sleep 5\n"), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Render(ctx, "graph LR", 40); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline lost", err)
	}
	out := &limitedOutput{limit: 3}
	if _, err := out.Write([]byte("1234")); err == nil || out.Len() > 3 {
		t.Fatal("output exceeded cap")
	}
}

func TestMermaidRealHelper(t *testing.T) {
	helper := os.Getenv("PEEP_TEST_BINARY")
	if helper == "" {
		t.Skip("set PEEP_TEST_BINARY for actual renderer integration")
	}
	r := NewMermaid(helper)
	for _, source := range []string{"flowchart LR\n A[Browse] --> B{Media?}\n B --> C[Preview]", "sequenceDiagram\n Alice->>Bob: Hello\n Bob-->>Alice: Hi"} {
		im, err := r.Render(context.Background(), source, 80)
		if err != nil || im.Width < 50 || im.Height < 50 {
			t.Fatalf("render: %dx%d %v", im.Width, im.Height, err)
		}
	}
	if _, err := r.Render(context.Background(), "this is not a diagram", 80); err == nil {
		t.Fatal("invalid diagram accepted")
	}
}

func TestMermaidLayout(t *testing.T) {
	for _, d := range []*Document{{Kind: "MD", Source: "# Heading\n\n```mermaid\nflowchart LR\nA-->B\n```\n\nAfter."}, {Kind: "MMD", Source: "flowchart LR\nA-->B"}} {
		l, err := Render(context.Background(), d, 60, false)
		if err != nil || len(l.Assets) != 1 || l.Assets[0].ReplaceFrom < 1 {
			t.Fatalf("missing replaceable diagram: %+v %v", l, err)
		}
		raw, err := Render(context.Background(), d, 60, true)
		if err != nil || len(raw.Assets) != 0 {
			t.Fatal("source mode requested a diagram")
		}
	}
}
