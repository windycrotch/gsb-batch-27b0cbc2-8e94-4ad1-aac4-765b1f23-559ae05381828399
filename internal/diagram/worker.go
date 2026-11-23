// Package diagram renders PNGs using the compiled-in Mermaid library. The TUI
// invokes this in a child copy of peep so cancellation also stops rasterization.
package diagram

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	mermaid "github.com/bvolpato/mermaid-go-renderer"
	"golang.org/x/net/html"
)

const WorkerArgument = "__peep_mermaid_png_v1"
const Version = "0.13.0"
const MaxSource = 128 << 10
const MaxOutput = 20 << 20

//go:embed notices/MMDG-LICENSE.txt
var License string

//go:embed notices/MMDG-NOTICES.txt
var Notices string

func Worker(ctx context.Context, in io.Reader, out io.Writer) error {
	source, err := io.ReadAll(io.LimitReader(in, MaxSource+1))
	if err != nil {
		return fmt.Errorf("read Mermaid: %w", err)
	}
	if len(source) > MaxSource {
		return fmt.Errorf("Mermaid source exceeds 128 KiB")
	}
	child, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	opts := []mermaid.Option{mermaid.WithTheme("dark"), mermaid.WithBackgroundColor("#171e2d"), mermaid.WithPage(mermaid.PageMermaidCLI), mermaid.WithViewportWidth(1616), mermaid.WithTime(time.Now(), time.UTC)}
	result, err := mermaid.RenderContext(child, string(source), opts...)
	if err != nil {
		return fmt.Errorf("Mermaid layout: %w", err)
	}
	svg, err := fitSVG(result.SVG)
	if err != nil {
		return err
	}
	if err = child.Err(); err != nil {
		return err
	}
	data, err := mermaid.SVGToPNG(svg, opts...)
	if err != nil {
		return fmt.Errorf("Mermaid PNG: %w", err)
	}
	if err = child.Err(); err != nil {
		return err
	}
	if len(data) > MaxOutput {
		return fmt.Errorf("Mermaid PNG exceeds 20 MiB")
	}
	_, err = out.Write(data)
	return err
}

// Set an explicit bounded viewport before allocating raster pixels. The source
// viewBox preserves geometry; inline dimensions override responsive SVG styles.
func fitSVG(svg string) (string, error) {
	if len(svg) > 8<<20 {
		return "", fmt.Errorf("Mermaid SVG exceeds 8 MiB")
	}
	z := html.NewTokenizer(strings.NewReader(svg))
	offset := 0
	for {
		kind := z.Next()
		raw := z.Raw()
		start := offset
		offset += len(raw)
		if kind == html.ErrorToken {
			return "", fmt.Errorf("Mermaid output has no SVG root")
		}
		if kind != html.StartTagToken {
			continue
		}
		token := z.Token()
		if token.Data != "svg" {
			continue
		}
		var width, height float64
		for _, a := range token.Attr {
			if strings.EqualFold(a.Key, "viewBox") {
				parts := strings.Fields(strings.ReplaceAll(a.Val, ",", " "))
				if len(parts) == 4 {
					width, _ = strconv.ParseFloat(parts[2], 64)
					height, _ = strconv.ParseFloat(parts[3], 64)
				}
			}
		}
		if width <= 0 || height <= 0 || math.IsNaN(width) || math.IsNaN(height) || math.IsInf(width, 0) || math.IsInf(height, 0) {
			return "", fmt.Errorf("Mermaid SVG has invalid dimensions")
		}
		scale := math.Min(2, 1600/math.Max(width, height))
		w, h := math.Max(1, math.Round(width*scale)), math.Max(1, math.Round(height*scale))
		var attrs []html.Attribute
		var style string
		for _, a := range token.Attr {
			switch a.Key {
			case "width", "height":
				continue
			case "style":
				style = a.Val
			default:
				attrs = append(attrs, a)
			}
		}
		style += fmt.Sprintf(";width:%.0fpx!important;height:%.0fpx!important;min-width:0!important;min-height:0!important;max-width:none!important;max-height:none!important;", w, h)
		token.Attr = append(attrs, html.Attribute{Key: "width", Val: fmt.Sprint(w)}, html.Attribute{Key: "height", Val: fmt.Sprint(h)}, html.Attribute{Key: "style", Val: style})
		return svg[:start] + token.String() + svg[offset:], nil
	}
}
