package document

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"math"
	"net/url"
	"path/filepath"

	"github.com/lancekrogers/peep/internal/media"
	"github.com/lancekrogers/peep/internal/render"
)

type Image struct {
	PNG                          []byte
	Width, Height, Columns, Rows int
	Cells                        []string
}

func ResolveImage(documentPath, ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference")
	}
	if u.Scheme != "" || u.Host != "" {
		return "", fmt.Errorf("remote images are not fetched")
	}
	path := u.Path
	if path == "" {
		return "", fmt.Errorf("empty image reference")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(documentPath), path)
	}
	return filepath.Clean(path), nil
}
func FitImage(w, h, columns int) (int, int) {
	columns = max(1, min(columns, 120))
	rows := max(1, int(math.Ceil(float64(columns*h)/float64(max(1, w)*2))))
	if rows > 24 {
		rows = 24
		columns = max(1, min(columns, int(float64(rows*w*2)/float64(max(1, h)))))
	}
	return columns, rows
}
func ReadImage(ctx context.Context, path string, columns int) (Image, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return Image{}, err
	}
	f.Close()
	data, err := media.PreviewPNG(ctx, path)
	if err != nil {
		return Image{}, err
	}
	return ImageFromPNG(data, columns)
}
func ImageFromPNG(data []byte, columns int) (Image, error) {
	if len(data) > 20<<20 {
		return Image{}, fmt.Errorf("document image exceeds 20 MiB")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("image metadata: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 8<<20 {
		return Image{}, fmt.Errorf("image exceeds document pixel limit")
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("decode document image: %w", err)
	}
	cols, rows := FitImage(cfg.Width, cfg.Height, columns)
	f := media.Frame{Width: cols, Height: rows * 2, RGB: make([]byte, cols*rows*6)}
	for y := 0; y < f.Height; y++ {
		for x := 0; x < cols; x++ {
			r, g, b, a := im.At(x*cfg.Width/cols, y*cfg.Height/f.Height).RGBA()
			at := (y*cols + x) * 3
			// Composite transparency onto the document panel background.
			f.RGB[at] = uint8((r + 23*(65535-a)/255) >> 8)
			f.RGB[at+1] = uint8((g + 30*(65535-a)/255) >> 8)
			f.RGB[at+2] = uint8((b + 45*(65535-a)/255) >> 8)
		}
	}
	return Image{PNG: data, Width: cfg.Width, Height: cfg.Height, Columns: cols, Rows: rows, Cells: render.Cells(f)}, nil
}
