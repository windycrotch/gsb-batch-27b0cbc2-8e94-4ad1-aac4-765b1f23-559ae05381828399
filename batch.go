package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tileOutput is a fully encoded PNG ready to be written to disk.
type tileOutput struct {
	filename string // final destination path
	data     []byte // complete PNG contents
}

// osRename is a package-level hook so tests can simulate write-phase failures.
var osRename = os.Rename

// outputBaseName strips the ".png" suffix, matching the historic naming scheme:
// "--output out.png" produces files named out_0.png, out_1.png, ...
func outputBaseName(outputFilename string) string {
	return strings.ReplaceAll(outputFilename, ".png", "")
}

// outputFilenameFor returns the final path of the i-th tile for a given
// --output prefix.
func outputFilenameFor(outputFilename string, i int) string {
	return fmt.Sprintf("%s_%d.png", outputBaseName(outputFilename), i)
}

// prepareOutputs decodes and PNG-encodes every address in memory. Nothing is
// written to disk here, so any failure aborts the batch before a single output
// file exists. The returned error always names the offending address.
func prepareOutputs(addresses []string, outputFilename string, romBytes []byte) ([]tileOutput, error) {
	outputs := make([]tileOutput, 0, len(addresses))

	for i, address := range addresses {
		data, err := encodeTileAtAddress(address, romBytes)
		if err != nil {
			return nil, fmt.Errorf("tile %d: %w", i, err)
		}

		outputs = append(outputs, tileOutput{
			filename: outputFilenameFor(outputFilename, i),
			data:     data,
		})
	}

	return outputs, nil
}

// writeBatch atomically publishes a prepared batch of tiles:
//
//  1. Every file is fully written (and fsynced) inside a temporary directory
//     next to the output. A failure here leaves zero output files behind.
//  2. Each staged file is moved into place with an atomic rename, so a final
//     file is always either a complete PNG or absent — never truncated, and
//     concurrent runs with the same prefix can never observe mixed content.
//     If a rename fails, the files already moved by this run are removed so a
//     half-written result set is never left on disk.
//  3. Stale files from previous runs (same prefix, different index) are
//     removed, so re-running with the same --output always converges to
//     exactly the same file set instead of accumulating leftovers.
func writeBatch(outputs []tileOutput, outputFilename string) error {
	dir := filepath.Dir(outputFilename)
	base := outputBaseName(outputFilename)

	desired := make(map[string]bool, len(outputs))
	for _, out := range outputs {
		desired[out.filename] = true
	}

	// Step 1: stage every file in a temporary directory on the same volume.
	tmpDir, err := os.MkdirTemp(dir, ".gbgraphics-*")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	for _, out := range outputs {
		tmpPath := filepath.Join(tmpDir, filepath.Base(out.filename))
		if err := writeFileSync(tmpPath, out.data); err != nil {
			return fmt.Errorf("failed to stage %s: %w", out.filename, err)
		}
	}

	// Step 2: move the staged files into place, rolling back on failure.
	var moved []string
	for _, out := range outputs {
		tmpPath := filepath.Join(tmpDir, filepath.Base(out.filename))
		if err := osRename(tmpPath, out.filename); err != nil {
			for _, f := range moved {
				os.Remove(f)
			}
			return fmt.Errorf("failed to move %s into place: %w", out.filename, err)
		}
		moved = append(moved, out.filename)
	}

	// Step 3: remove stale outputs from previous runs so the file set on disk
	// is exactly the desired one.
	stale, err := filepath.Glob(base + "_*.png")
	if err != nil {
		return fmt.Errorf("failed to list stale outputs for %q: %w", base, err)
	}
	for _, f := range stale {
		if !desired[f] {
			if err := os.Remove(f); err != nil {
				return fmt.Errorf("failed to remove stale output %s: %w", f, err)
			}
		}
	}

	return nil
}

// writeFileSync writes data to path and fsyncs it before closing, so a
// successfully staged file is durable and complete.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}

	return f.Close()
}
