package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"runtime"
)

// loadIcon reads the tray icon from the resource locations.
//
// A failure is not fatal: the caller falls back to a generated icon so the tray
// still appears, and reports the problem to the user.
func loadIcon() ([]byte, error) {
	path, err := findResource("icon.png")
	if err != nil {
		return nil, fmt.Errorf("tray icon unavailable, using a generated placeholder: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading tray icon %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("reading tray icon %s: file is empty", path)
	}

	// The Windows tray API only loads .ico files, while the shipped icon (and
	// the macOS/Linux tray APIs) are PNG. Repackage the PNG into an ICO
	// container so the same file works on every platform.
	if runtime.GOOS == "windows" && !isICO(data) {
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("reading tray icon %s: %w", path, err)
		}
		return wrapPNGInICO(data, cfg.Width, cfg.Height), nil
	}
	return data, nil
}

func isICO(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == 0 && data[1] == 0 && data[2] == 1 && data[3] == 0
}

// fallbackIcon draws a simple clock face so the tray is always visible, even
// when no icon file ships with the binary.
func fallbackIcon() []byte {
	const size = 32

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	paper := color.RGBA{R: 0xF2, G: 0xF2, B: 0xF2, A: 0xFF}
	ink := color.RGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xFF}

	center := image.Point{X: size / 2, Y: size / 2}
	radius := size/2 - 1

	// Filled disc.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-center.X, y-center.Y
			if dx*dx+dy*dy <= radius*radius {
				img.Set(x, y, paper)
			}
		}
	}

	// Minute and hour hands.
	drawLine(img, center, image.Point{X: center.X, Y: center.Y - radius + 3}, ink)
	drawLine(img, center, image.Point{X: center.X + radius - 8, Y: center.Y}, ink)

	data, err := encodeForTray(img)
	if err != nil {
		// A PNG is still better than nothing; some platforms accept it.
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		return buf.Bytes()
	}
	return data
}

// encodeForTray encodes img as PNG, wrapped in an ICO container on Windows where
// the tray API expects icon files.
func encodeForTray(img image.Image) ([]byte, error) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		return pngBuf.Bytes(), nil
	}
	return wrapPNGInICO(pngBuf.Bytes(), img.Bounds().Dx(), img.Bounds().Dy()), nil
}

// wrapPNGInICO builds a one-image ICO file around PNG data (supported since
// Windows Vista). Older Windows versions are not targeted by this program.
func wrapPNGInICO(pngData []byte, width, height int) []byte {
	const iconDirHeaderSize = 6
	const iconDirEntrySize = 16
	const pngImageOffset = iconDirHeaderSize + iconDirEntrySize

	var buf bytes.Buffer
	buf.Grow(pngImageOffset + len(pngData))

	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count

	// ICONDIRENTRY; a dimension of 256 is stored as 0.
	dim := func(n int) byte {
		if n >= 256 {
			return 0
		}
		return byte(n)
	}
	buf.WriteByte(dim(width))
	buf.WriteByte(dim(height))
	buf.WriteByte(0)                                   // palette size
	buf.WriteByte(0)                                   // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // colour planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, uint32(len(pngData)))
	binary.Write(&buf, binary.LittleEndian, uint32(pngImageOffset))

	buf.Write(pngData)
	return buf.Bytes()
}

func drawLine(img *image.RGBA, start, end image.Point, col color.Color) {
	dx := abs(end.X - start.X)
	dy := abs(end.Y - start.Y)
	sx, sy := 1, 1
	if start.X >= end.X {
		sx = -1
	}
	if start.Y >= end.Y {
		sy = -1
	}
	err := dx - dy

	for {
		img.Set(start.X, start.Y, col)
		if start.X == end.X && start.Y == end.Y {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			start.X += sx
		}
		if e2 < dx {
			err += dx
			start.Y += sy
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
