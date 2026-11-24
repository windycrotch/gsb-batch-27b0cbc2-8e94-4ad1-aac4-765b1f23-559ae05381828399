package main

import (
	"fmt"
	"image"
	_ "image/png"
	"os"
)

func readImageFromFilePath(path string) (image.Image, error) {
	// Load GB ROM
	infile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open screenshot %q: %w", path, err)
	}

	defer func(infile *os.File) {
		if err := infile.Close(); err != nil {
			fmt.Println(err)
		}
	}(infile)

	// Decode the image
	imData, imType, err := image.Decode(infile)
	if err != nil {
		return nil, fmt.Errorf("failed to decode screenshot %q: %w", path, err)
	}

	// Make sure it's a PNG
	if imType != "png" {
		return nil, fmt.Errorf("screenshot %q is not a PNG (it is %q)", path, imType)
	}

	return imData, nil
}

func saveUniqueTiles(tiles []image.Image, tilesPerRow int, numRows int, filename string) ([]image.Image, error) {
	uniqueTiles := []image.Image{}

	for _, tile := range tiles {
		isUnique := true
		for j := 0; j < len(uniqueTiles); j++ {
			if areImagesEquivalent(tile, uniqueTiles[j]) {
				isUnique = false
				break
			}
		}

		if isUnique {
			uniqueTiles = append(uniqueTiles, tile)
		}
	}

	return uniqueTiles, nil
}
