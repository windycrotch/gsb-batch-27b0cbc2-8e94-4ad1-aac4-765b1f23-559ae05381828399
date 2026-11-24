package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
)

// Takes a 8x8 PNG RGBA images and converts it to 2BPP
// and returns the original byte array (what the GB rom would contain, and you could see with a hex editor)
func pngTo2BPP(imData image.Image) ([]byte, error) {
	// Make sure it is 8x8
	if imData.Bounds().Dx() != 8 || imData.Bounds().Dy() != 8 {
		return nil, fmt.Errorf("tile is not 8x8, it is %dx%d", imData.Bounds().Dx(), imData.Bounds().Dy())
	}

	if err := checkColor(imData); err != nil {
		return nil, err
	}

	// print image pixel color r,g,b,a into uint32
	var binCode []byte

	for y := 0; y < imData.Bounds().Max.Y; y++ {
		var binLow, binHigh uint8

		for x := 0; x < imData.Bounds().Max.X; x++ {
			var lowBit, highBit uint8

			pixelColor := imData.At(x, y)

			// type assertion must be checked
			col, ok := pixelColor.(color.RGBA)
			if !ok {
				return nil, fmt.Errorf("pixel (%d,%d) is not RGBA", x, y)
			}

			r := col.R
			g := col.G
			b := col.B

			if r == g && g == b {
				switch r {
				case 0: // black, r = 3
					highBit = 1
					lowBit = 1
				case 85: // dark gray, r = 2
					highBit = 1
					lowBit = 0
				case 170: // light gray, r = 1
					highBit = 0
					lowBit = 1
				case 255: // white, r = 0
					highBit = 0
					lowBit = 0
				default:
					return nil, fmt.Errorf("unknown colour (r=%d g=%d b=%d) at pixel (%d,%d): not in palette", r, g, b, x, y)
				}
			} else {
				// Find the closest palette colour
				if red, green, blue := GetPaletteColour(darkest, PaletteBGB); r == red && g == green && b == blue {
					highBit = 1
					lowBit = 1
				} else if red, green, blue := GetPaletteColour(dark, PaletteBGB); r == red && g == green && b == blue {
					highBit = 1
					lowBit = 0
				} else if red, green, blue := GetPaletteColour(light, PaletteBGB); r == red && g == green && b == blue {
					highBit = 0
					lowBit = 1
				} else if red, green, blue := GetPaletteColour(lightest, PaletteBGB); r == red && g == green && b == blue {
					highBit = 0
					lowBit = 0
				} else {
					return nil, fmt.Errorf("unknown colour (r=%d g=%d b=%d) at pixel (%d,%d): not in palette", r, g, b, x, y)
				}
			}

			binLow += lowBit * uint8(math.Pow(2, float64(7-x)))
			binHigh += highBit * uint8(math.Pow(2, float64(7-x)))
		}

		binCode = append(binCode, binLow, binHigh)
	}

	return binCode, nil
}
