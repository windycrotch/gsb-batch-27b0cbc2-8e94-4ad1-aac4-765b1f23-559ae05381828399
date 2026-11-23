package document

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalImageReferences(t *testing.T) {
	path, err := ResolveImage("/docs/readme.md", "assets/a%20b.png")
	if err != nil || path != filepath.FromSlash("/docs/assets/a b.png") {
		t.Fatalf("%s: %v", path, err)
	}
	for _, ref := range []string{"https://example.com/a.png", "//example.com/a.png", "data:image/png;base64,abc", "file:///a.png", ""} {
		if _, err := ResolveImage("/docs/readme.md", ref); err == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
	if _, err := ReadImage(context.Background(), filepath.Join(t.TempDir(), "missing.png"), 40); err == nil {
		t.Fatal("missing image accepted")
	}
}

func TestImagePixelsAndLimits(t *testing.T) {
	im := image.NewNRGBA(image.Rect(0, 0, 12, 8))
	im.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	got, err := ImageFromPNG(b.Bytes(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 12 || got.Height != 8 || !bytes.Equal(got.PNG, b.Bytes()) || len(got.Cells) != 4 {
		t.Fatalf("bad image: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Cells, ""), "23;30;45") {
		t.Fatal("transparent pixels lack panel background")
	}
	if _, err := ImageFromPNG(make([]byte, 21<<20), 20); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := ImageFromPNG([]byte("broken"), 20); err == nil {
		t.Fatal("bad PNG accepted")
	}
	for _, size := range [][2]int{{4096, 1}, {1, 4096}, {100, 100}} {
		c, r := FitImage(size[0], size[1], 400)
		if c < 1 || c > 120 || r < 1 || r > 24 {
			t.Fatalf("unbounded layout %dx%d", c, r)
		}
	}
}
