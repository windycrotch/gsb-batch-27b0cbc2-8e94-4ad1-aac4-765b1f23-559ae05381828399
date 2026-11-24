package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

func getTiles(img image.Image, romBytes []byte, numColumns int) ([]string, error) {
	// Step 4: Split the image into sub-images
	var tiles []image.Image
	var err error

	if numColumns == 0 {
		tiles, err = split8x8(img) // Split the image into sub-images of 8x8 pixels (tiles) and store them in a slice
	} else {
		tiles, err = split7x8(img) // Split the image into sub-images of 7x8 pixels (tiles) and store them in a slice
	}

	if err != nil {
		return nil, err
	}

	// Step 6: From the all the tiles, remove the duplicates
	uniqueTiles, err := saveUniqueTiles(tiles, gbScreenXRes, tilesPerCol, fmt.Sprintf("%d", numColumns+1))
	if err != nil {
		return nil, err
	}

	// Step 7:  These tiles are in RGBA format, so we need to convert them to 2BPP
	// 			before we can compare them to the original gameboy tileset
	origCodeTiles, err := getHexCodes(uniqueTiles)
	if err != nil {
		return nil, err
	}
	uniqCodeTiles := removeDuplicateByte(origCodeTiles)

	// Step 8: Search for each tile in the screenshot and return the addresses from the ROM
	return findTileAddresses(uniqCodeTiles, romBytes), nil
}

// renderTile renders a 2BPP tile as an image (white background)
func renderTile(tile []byte) image.Image {
	// Calculate the height of the img in bytes
	height := 8 * int(math.Ceil(float64(len(tile))/float64(8*8*bitDepth)))
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.White)
		}
	}

	// Modify the img
	convert2BPPToPNG(height, tile, img, 0, 0)

	return img
}

// encodeTileAtAddress decodes the tile stored at the given hex address in the
// ROM and returns it as an encoded PNG. Nothing is written to disk. Any
// failure (bad address, out-of-bounds range, short tile) is reported with the
// offending address so the caller can abort the whole batch.
func encodeTileAtAddress(address string, romBytes []byte) ([]byte, error) {
	rangeStartOffset, err := convertHexToInt32(address)
	if err != nil {
		return nil, fmt.Errorf("address %s: %w", address, err)
	}

	if rangeStartOffset < 0 {
		return nil, fmt.Errorf("address %s: negative start offset %d", address, rangeStartOffset)
	}

	if int64(rangeStartOffset)+int64(rangeLength) > int64(len(romBytes)) {
		return nil, fmt.Errorf("address %s: tile range [%d, %d) is out of bounds for a ROM of %d bytes",
			address, rangeStartOffset, int64(rangeStartOffset)+int64(rangeLength), len(romBytes))
	}

	tile := romBytes[rangeStartOffset : rangeStartOffset+rangeLength]

	img := renderTile(tile)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("address %s: failed to encode PNG: %w", address, err)
	}

	return buf.Bytes(), nil
}

func split8x8(src image.Image) ([]image.Image, error) {
	// Check if image resolution is 160x144
	if src.Bounds().Dx() != gbScreenXRes || src.Bounds().Dy() != gbScreenYRes {
		return nil, fmt.Errorf("screenshot is not %dx%d, it is %dx%d",
			gbScreenXRes, gbScreenYRes, src.Bounds().Dx(), src.Bounds().Dy())
	}

	if err := checkColor(src); err != nil {
		return nil, err
	}

	// Iterate over the image pixels and split it into 8x8 sub-images
	var tiles []image.Image

	for y := 0; y < src.Bounds().Max.Y; y += 8 {
		for x := 0; x < src.Bounds().Max.X; x += 8 {
			tile := image.NewRGBA(image.Rect(0, 0, 8, 8))

			for i := 0; i < 8; i++ {
				for j := 0; j < 8; j++ {
					tile.Set(i, j, src.At(x+i, y+j))
				}
			}

			tiles = append(tiles, tile)
		}
	}

	return tiles, nil
}

func split7x8(src image.Image) ([]image.Image, error) {
	if err := checkColor(src); err != nil {
		return nil, err
	}

	// Iterate over the image pixels and split it into 7x8 sub-images
	var tiles []image.Image

	// Skip the last tile of every row
	for y := 0; y < src.Bounds().Max.Y; y += 8 {
		for x := 0; x < src.Bounds().Max.X-8; x += 8 {
			tile := image.NewRGBA(image.Rect(0, 0, 8, 8))

			for i := 0; i < 8; i++ {
				for j := 0; j < 8; j++ {
					tile.Set(i, j, src.At(x+i, y+j))
				}
			}

			tiles = append(tiles, tile)
		}
	}

	return tiles, nil
}

// Function to find the addresses of tiles in the ROM
func findTileAddresses(uniqCodeTiles [][]byte, romBytes []byte) []string {
	var addr []string

	// Search for each tile in the screenshot
	for _, tile := range uniqCodeTiles {
		// Calculate the maximum index in romBytes where a match with tile can start
		// This is to prevent an "out of bounds" error when accessing the slice romBytes[j:j+len(tile)]
		maxIndex := len(romBytes) - len(tile) + 1

		// Search for the tile in the rom
		for j := 0; j < maxIndex; j++ {
			// Get a slice of romBytes that has the same length as tile
			// This slice will be compared with tile to check for a match
			romSlice := romBytes[j : j+len(tile)]

			if compare(tile, romSlice) {
				// If a match is found, convert the index of the match to a hexadecimal string
				// and append it to the addr slice
				addr = append(addr, fmt.Sprintf("0x%X", j))

				// No reason to keep searching for this tile if you have already found it
				break
			}
		}
	}

	return addr
}
