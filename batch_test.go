package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- test helpers ---------------------------------------------------------

// writeScreenshot creates an RGBA PNG screenshot of the given size. Every
// pixel is white unless paint says otherwise.
func writeScreenshot(t *testing.T, path string, w, h int, paint func(x, y int) (color.RGBA, bool)) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: 255, G: 255, B: 255, A: 255}
			if paint != nil {
				if pc, ok := paint(x, y); ok {
					c = pc
				}
			}
			img.Set(x, y, c)
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create screenshot: %v", err)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode screenshot: %v", err)
	}

	return path
}

// writeTestROM writes a ROM filled with 0xFF, with a 16-byte zero tile
// planted at 0x100.
func writeTestROM(t *testing.T, path string) []byte {
	t.Helper()

	rom := bytes.Repeat([]byte{0xFF}, 0x400)
	// 16 zero bytes (an all-white tile) at 0x100
	for i := 0x100; i < 0x110; i++ {
		rom[i] = 0x00
	}

	if err := os.WriteFile(path, rom, 0644); err != nil {
		t.Fatalf("write ROM: %v", err)
	}

	return rom
}

// setupGame creates a ROM and a 160x144 screenshot whose top-left 8x8 tile is
// black and everything else is white. Expected hits: the all-0xFF tile at
// 0x0 and the all-0x00 tile at 0x100.
func setupGame(t *testing.T, dir string) args {
	t.Helper()

	romPath := filepath.Join(dir, "game.gb")
	writeTestROM(t, romPath)

	black := color.RGBA{R: 0, G: 0, B: 0, A: 255}
	imgPath := writeScreenshot(t, filepath.Join(dir, "shot.png"), gbScreenXRes, gbScreenYRes,
		func(x, y int) (color.RGBA, bool) {
			if x < 8 && y < 8 {
				return black, true
			}
			return color.RGBA{}, false
		})

	return args{
		Rom:        romPath,
		Screenshot: imgPath,
		Output:     filepath.Join(dir, "out.png"),
	}
}

func listOutputFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "out_") {
			files = append(files, e.Name())
		}
	}

	return files
}

func assertValidPNG(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s is not a valid PNG: %v", path, err)
	}

	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 8 {
		t.Fatalf("%s: expected 8x8 tile, got %v", path, img.Bounds())
	}
}

// --- happy path -----------------------------------------------------------

func TestRunSuccessProducesExpectedFileSet(t *testing.T) {
	dir := t.TempDir()
	userInput := setupGame(t, dir)

	if err := run(userInput); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	files := listOutputFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("expected 2 output files, got %v", files)
	}

	out0 := filepath.Join(dir, "out_0.png")
	out1 := filepath.Join(dir, "out_1.png")
	assertValidPNG(t, out0)
	assertValidPNG(t, out1)

	// out_0 is the all-black tile (0xFF bytes -> colour 3 -> black pixels).
	data, err := os.ReadFile(out0)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(4, 4).RGBA()
	if r != 0 || g != 0 || b != 0 {
		t.Fatalf("out_0.png should be all black, got (%d,%d,%d)", r, g, b)
	}

	// out_1 is the all-white tile.
	data, err = os.ReadFile(out1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ = img.At(4, 4).RGBA()
	if r != 0xffff || g != 0xffff || b != 0xffff {
		t.Fatalf("out_1.png should be all white, got (%d,%d,%d)", r, g, b)
	}
}

func TestRunIsDeterministicAndCleansStaleFiles(t *testing.T) {
	dir := t.TempDir()
	userInput := setupGame(t, dir)

	if err := run(userInput); err != nil {
		t.Fatalf("first run failed: %v", err)
	}

	first := map[string][]byte{}
	for _, name := range listOutputFiles(t, dir) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		first[name] = data
	}

	// Plant stale leftovers from a "previous" run: a higher index that no
	// longer belongs to the result set, and a corrupted out_0.png.
	stale := filepath.Join(dir, "out_7.png")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out_0.png"), []byte("garbage"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := run(userInput); err != nil {
		t.Fatalf("second run failed: %v", err)
	}

	files := listOutputFiles(t, dir)
	if len(files) != len(first) {
		t.Fatalf("expected same file set %v, got %v", keys(first), files)
	}

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		expected, ok := first[name]
		if !ok {
			t.Fatalf("unexpected extra file %s", name)
		}
		if !bytes.Equal(data, expected) {
			t.Fatalf("file %s differs between identical runs", name)
		}
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file %s should have been removed", stale)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- validation failures: error before any file is written ----------------

func TestRunFailsOnMissingROM(t *testing.T) {
	dir := t.TempDir()
	userInput := args{
		Rom:        filepath.Join(dir, "does-not-exist.gb"),
		Screenshot: writeScreenshot(t, filepath.Join(dir, "shot.png"), gbScreenXRes, gbScreenYRes, nil),
		Output:     filepath.Join(dir, "out.png"),
	}

	if err := run(userInput); err == nil {
		t.Fatal("expected an error for a missing ROM")
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

func TestRunFailsOnMissingScreenshot(t *testing.T) {
	dir := t.TempDir()
	romPath := filepath.Join(dir, "game.gb")
	writeTestROM(t, romPath)

	userInput := args{
		Rom:        romPath,
		Screenshot: filepath.Join(dir, "does-not-exist.png"),
		Output:     filepath.Join(dir, "out.png"),
	}

	if err := run(userInput); err == nil {
		t.Fatal("expected an error for a missing screenshot")
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

func TestRunFailsOnCorruptScreenshot(t *testing.T) {
	dir := t.TempDir()
	romPath := filepath.Join(dir, "game.gb")
	writeTestROM(t, romPath)

	bad := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(bad, []byte("this is not a png"), 0644); err != nil {
		t.Fatal(err)
	}

	userInput := args{Rom: romPath, Screenshot: bad, Output: filepath.Join(dir, "out.png")}

	if err := run(userInput); err == nil {
		t.Fatal("expected an error for a corrupt screenshot")
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

func TestRunFailsOnWrongScreenshotSize(t *testing.T) {
	dir := t.TempDir()
	romPath := filepath.Join(dir, "game.gb")
	writeTestROM(t, romPath)

	imgPath := writeScreenshot(t, filepath.Join(dir, "shot.png"), 100, 100, nil)

	userInput := args{Rom: romPath, Screenshot: imgPath, Output: filepath.Join(dir, "out.png")}

	err := run(userInput)
	if err == nil {
		t.Fatal("expected an error for a non-160x144 screenshot")
	}
	if !strings.Contains(err.Error(), "160x144") {
		t.Fatalf("error should mention the expected dimensions, got: %v", err)
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

func TestRunFailsOnColourOutsidePalette(t *testing.T) {
	dir := t.TempDir()
	romPath := filepath.Join(dir, "game.gb")
	writeTestROM(t, romPath)

	badPixel := color.RGBA{R: 1, G: 2, B: 3, A: 255}
	imgPath := writeScreenshot(t, filepath.Join(dir, "shot.png"), gbScreenXRes, gbScreenYRes,
		func(x, y int) (color.RGBA, bool) {
			if x == 0 && y == 0 {
				return badPixel, true
			}
			return color.RGBA{}, false
		})

	userInput := args{Rom: romPath, Screenshot: imgPath, Output: filepath.Join(dir, "out.png")}

	err := run(userInput)
	if err == nil {
		t.Fatal("expected an error for a colour outside the palette")
	}
	if !strings.Contains(err.Error(), "not in palette") {
		t.Fatalf("error should mention the palette, got: %v", err)
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

// --- out-of-bounds / short tiles: error naming the address, no panic ------

func TestPrepareOutputsFailsOnOutOfBoundsAddress(t *testing.T) {
	dir := t.TempDir()
	rom := bytes.Repeat([]byte{0xAB}, 0x400)
	output := filepath.Join(dir, "out.png")

	cases := []struct {
		name    string
		address string
	}{
		{"way past the end", "0xFFFFFF"},
		{"tile truncated at ROM end", "0x3F8"}, // 0x3F8 + 16 > 0x400
		{"negative offset", "0x-1"},
		{"not hex", "not-an-address"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outputs, err := prepareOutputs([]string{"0x0", tc.address}, output, rom)
			if err == nil {
				t.Fatalf("expected an error for address %s", tc.address)
			}
			if outputs != nil {
				t.Fatalf("no outputs should be returned on failure, got %v", outputs)
			}
			if !strings.Contains(err.Error(), tc.address) {
				t.Fatalf("error should name the offending address %s, got: %v", tc.address, err)
			}
		})
	}

	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no output files should exist after failure, got %v", files)
	}
}

func TestEncodeTileAtAddressDoesNotPanicOnShortROM(t *testing.T) {
	// ROM smaller than one tile.
	rom := []byte{0x00, 0x01, 0x02}

	if _, err := encodeTileAtAddress("0x0", rom); err == nil {
		t.Fatal("expected an error for a ROM shorter than one tile")
	}
}

// --- write phase: atomicity, rollback, recovery ---------------------------

func preparedBatch(t *testing.T, dir string, count int) ([]tileOutput, string) {
	t.Helper()

	rom := bytes.Repeat([]byte{0xAB}, 0x400)
	addresses := make([]string, count)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("0x%X", i*0x10)
	}

	output := filepath.Join(dir, "out.png")
	outputs, err := prepareOutputs(addresses, output, rom)
	if err != nil {
		t.Fatalf("prepareOutputs: %v", err)
	}

	return outputs, output
}

func TestWriteBatchFailureLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	outputs, output := preparedBatch(t, dir, 3)

	// Simulate an interruption: the second rename of the batch fails.
	calls := 0
	original := osRename
	osRename = func(oldpath, newpath string) error {
		calls++
		if calls == 2 {
			return errors.New("simulated write-phase failure")
		}
		return original(oldpath, newpath)
	}
	t.Cleanup(func() { osRename = original })

	if err := writeBatch(outputs, output); err == nil {
		t.Fatal("expected writeBatch to fail")
	}

	// The file moved before the failure must have been rolled back.
	if files := listOutputFiles(t, dir); len(files) != 0 {
		t.Fatalf("no partial output should remain after a failed batch, got %v", files)
	}

	// No staging directories may leak either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gbgraphics-") {
			t.Fatalf("staging directory %s was left behind", e.Name())
		}
	}
}

func TestRunRecoversAfterInterruptedBatch(t *testing.T) {
	dir := t.TempDir()
	outputs, output := preparedBatch(t, dir, 3)

	// First attempt: every rename fails (task interrupted during the write phase).
	original := osRename
	osRename = func(oldpath, newpath string) error {
		return errors.New("simulated interruption")
	}
	if err := writeBatch(outputs, output); err == nil {
		t.Fatal("expected writeBatch to fail")
	}
	osRename = original
	t.Cleanup(func() { osRename = original })

	// Second attempt with the same parameters must produce the complete set.
	if err := writeBatch(outputs, output); err != nil {
		t.Fatalf("rerun after interruption failed: %v", err)
	}

	files := listOutputFiles(t, dir)
	if len(files) != 3 {
		t.Fatalf("expected the complete 3-file set after recovery, got %v", files)
	}
	for i := 0; i < 3; i++ {
		assertValidPNG(t, filepath.Join(dir, fmt.Sprintf("out_%d.png", i)))
	}
}

// --- concurrency ----------------------------------------------------------

func TestConcurrentBatchesSamePrefix(t *testing.T) {
	dir := t.TempDir()
	outputs, output := preparedBatch(t, dir, 4)

	expected := make(map[string][]byte, len(outputs))
	for _, out := range outputs {
		expected[filepath.Base(out.filename)] = out.data
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeBatch(outputs, output); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent writeBatch failed: %v", err)
	}

	// Every final file must be a complete, valid PNG with the exact expected
	// content — never truncated, never a mix of two writers.
	files := listOutputFiles(t, dir)
	if len(files) != len(expected) {
		t.Fatalf("expected %d files, got %v", len(expected), files)
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		assertValidPNG(t, path)

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, expected[name]) {
			t.Fatalf("file %s has unexpected (mixed?) content", name)
		}
	}

	// No staging directories may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gbgraphics-") {
			t.Fatalf("staging directory %s was left behind", e.Name())
		}
	}
}

func TestConcurrentFullRunsSamePrefix(t *testing.T) {
	dir := t.TempDir()
	userInput := setupGame(t, dir)

	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(userInput); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent run failed: %v", err)
	}

	files := listOutputFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("expected exactly 2 output files, got %v", files)
	}
	for _, name := range files {
		assertValidPNG(t, filepath.Join(dir, name))
	}
}
