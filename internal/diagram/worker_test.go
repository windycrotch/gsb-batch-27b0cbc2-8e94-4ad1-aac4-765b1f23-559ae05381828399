package diagram

import (
	"bytes"
	"context"
	"image/png"
	"strings"
	"testing"
)

func TestWorkerRendersWithoutTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, source := range []string{"flowchart LR\nA[Start]-->B[Done]", "sequenceDiagram\nAlice->>Bob: Hello", "classDiagram\n Animal <|-- Duck"} {
		var out bytes.Buffer
		if err := Worker(context.Background(), strings.NewReader(source), &out); err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width < 50 || cfg.Height < 50 || cfg.Width > 1600 || cfg.Height > 1600 {
			t.Fatal("invalid dimensions", cfg)
		}
	}
}
func TestWorkerRejectsInvalidAndLargeInput(t *testing.T) {
	for _, source := range []string{"not a diagram", strings.Repeat("x", MaxSource+1)} {
		var out bytes.Buffer
		if err := Worker(context.Background(), strings.NewReader(source), &out); err == nil {
			t.Fatal("accepted invalid input")
		}
		if out.Len() != 0 {
			t.Fatal("wrote partial PNG")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Worker(ctx, strings.NewReader("flowchart LR\nA-->B"), &bytes.Buffer{}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestSVGViewportBound(t *testing.T) {
	svg, err := fitSVG(`<svg viewBox="0 0 20000 10000" width="100%" style="max-width:20000px"><g/></svg>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svg, `width="1600"`) || !strings.Contains(svg, `height="800"`) {
		t.Fatal(svg)
	}
	for _, svg := range []string{`<svg viewBox="0 0 NaN 10"></svg>`, `<svg viewBox="0 0 +Inf 10"></svg>`, `<svg></svg>`} {
		if _, err := fitSVG(svg); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
}
