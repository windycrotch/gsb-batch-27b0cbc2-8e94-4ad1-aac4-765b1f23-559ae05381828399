package stegano

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	c "github.com/scott-mescudi/stegano/compression"
)

// encryptOverhead is the fixed number of bytes u.Encrypt adds to its input:
// 16 bytes of salt, 12 bytes of GCM nonce and 16 bytes of GCM tag.
const encryptOverhead = 16 + 12 + 16

// writeCoverPNG generates a deterministic cover image and saves it as a PNG.
func writeCoverPNG(t *testing.T, path string, width, height int) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + y*3) % 256),
				G: uint8((x*5 + y*11) % 256),
				B: uint8((x*13 + y*17) % 256),
				A: 255,
			})
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create cover image: %v", err)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		t.Fatalf("failed to encode cover image: %v", err)
	}
}

// deterministicBytes returns n pseudo-random (incompressible) bytes from a
// fixed seed, so tests are reproducible across runs.
func deterministicBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	if _, err := r.Read(b); err != nil {
		panic(err)
	}
	return b
}

// embeddedPayloadLen returns the exact number of bytes EmbedFile will write
// into the image for the given raw payload (header + file contents):
// the zstd-compressed length plus the fixed encryption overhead.
func embeddedPayloadLen(t *testing.T, payload []byte) int {
	t.Helper()

	compressed, err := c.CompressZSTD(payload)
	if err != nil {
		t.Fatalf("failed to compress payload: %v", err)
	}
	return len(compressed) + encryptOverhead
}

// dataFileHeader mirrors the filename header EmbedFile prepends to the data.
func dataFileHeader(dataFilePath string) string {
	return fmt.Sprintf("/-%s-/\n", filepath.Base(dataFilePath))
}

// findDataForPayloadSize returns deterministic file contents whose final
// embedded payload (header + data, compressed and encrypted) is exactly
// targetLen bytes.
func findDataForPayloadSize(t *testing.T, header string, targetLen int, seed int64) []byte {
	t.Helper()

	base := targetLen - encryptOverhead - len(header)
	for n := base - 128; n <= base+128; n++ {
		if n <= 0 {
			continue
		}
		data := deterministicBytes(seed, n)
		if embeddedPayloadLen(t, append([]byte(header), data...)) == targetLen {
			return data
		}
	}

	t.Fatalf("could not construct data with embedded payload of %d bytes", targetLen)
	return nil
}

// extractAndRead runs ExtractFile in a fresh directory and returns the
// extracted file's contents.
func extractAndRead(t *testing.T, stegoPath, password, extractedName string, bitDepth uint8) []byte {
	t.Helper()

	extractDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(extractDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("failed to restore working directory: %v", err)
		}
	}()

	if err := ExtractFile(stegoPath, password, bitDepth); err != nil {
		t.Fatalf("ExtractFile failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(extractDir, extractedName))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	return got
}

func TestEmbedFile_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, 128, 128)

	want := []byte("hello steganography: this is a secret file payload.\n\x00\x01\x02 binary too")
	dataPath := filepath.Join(tmp, "payload.bin")
	if err := os.WriteFile(dataPath, want, 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	for _, bitDepth := range []uint8{0, 1, 3} {
		out := filepath.Join(tmp, fmt.Sprintf("stego_%d.png", bitDepth))
		if err := EmbedFile(cover, dataPath, out, "s3cret", bitDepth); err != nil {
			t.Fatalf("EmbedFile(bitDepth=%d) failed: %v", bitDepth, err)
		}

		got := extractAndRead(t, out, "s3cret", "payload.bin", bitDepth)
		if !bytes.Equal(got, want) {
			t.Fatalf("bitDepth=%d: round trip mismatch, got %d bytes, want %d", bitDepth, len(got), len(want))
		}
	}
}

func TestEmbedFile_CapacityExactBoundary(t *testing.T) {
	tmp := t.TempDir()

	const bitDepth = 7
	const width, height = 40, 40
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, width, height)

	// Capacity in bits; the embedded payload also carries a 32-bit length header.
	capacityBits := width * height * 3 * (int(bitDepth) + 1)
	if (capacityBits-32)%8 != 0 {
		t.Fatalf("test setup: capacity %d bits cannot produce an exact byte fit", capacityBits)
	}
	exactPayload := (capacityBits - 32) / 8

	dataPath := filepath.Join(tmp, "data.bin")
	header := dataFileHeader(dataPath)

	// Exactly at capacity: must succeed and round trip.
	fitData := findDataForPayloadSize(t, header, exactPayload, 1)
	if err := os.WriteFile(dataPath, fitData, 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	out := filepath.Join(tmp, "stego.png")
	if err := EmbedFile(cover, dataPath, out, "pw", bitDepth); err != nil {
		t.Fatalf("exact-fit embed must succeed, got: %v", err)
	}

	got := extractAndRead(t, out, "pw", "data.bin", bitDepth)
	if !bytes.Equal(got, fitData) {
		t.Fatalf("exact-fit round trip mismatch, got %d bytes, want %d", len(got), len(fitData))
	}

	// One byte over capacity: must be rejected with ErrDataTooLarge.
	overData := findDataForPayloadSize(t, header, exactPayload+1, 2)
	if err := os.WriteFile(dataPath, overData, 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	out2 := filepath.Join(tmp, "stego_over.png")
	err := EmbedFile(cover, dataPath, out2, "pw", bitDepth)
	if !errors.Is(err, ErrDataTooLarge) {
		t.Fatalf("one byte over capacity must return ErrDataTooLarge, got: %v", err)
	}
	if _, statErr := os.Stat(out2); !os.IsNotExist(statErr) {
		t.Fatalf("rejected embed must not create an output file")
	}
}

func TestEmbedFile_TooLargePreservesExistingOutput(t *testing.T) {
	tmp := t.TempDir()
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, 32, 32)

	// Incompressible data far larger than a 32x32 image can hold.
	dataPath := filepath.Join(tmp, "big.bin")
	if err := os.WriteFile(dataPath, deterministicBytes(7, 100_000), 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	out := filepath.Join(tmp, "out.png")
	sentinel := []byte("pre-existing file contents that must survive")
	if err := os.WriteFile(out, sentinel, 0o644); err != nil {
		t.Fatalf("failed to write sentinel file: %v", err)
	}

	err := EmbedFile(cover, dataPath, out, "pw", 1)
	if !errors.Is(err, ErrDataTooLarge) {
		t.Fatalf("expected ErrDataTooLarge, got: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("existing output file was modified on capacity failure")
	}
}

func TestEmbedFile_ErrorIsPropagated(t *testing.T) {
	tmp := t.TempDir()
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, 16, 16)

	// Slightly-too-large data used to be silently swallowed by the
	// non-blocking channel check, returning nil and a corrupt file.
	dataPath := filepath.Join(tmp, "data.bin")
	if err := os.WriteFile(dataPath, deterministicBytes(11, 10_000), 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	out := filepath.Join(tmp, "out.png")
	for i := 0; i < 5; i++ {
		err := EmbedFile(cover, dataPath, out, "pw", 0)
		if err == nil {
			t.Fatalf("iteration %d: oversized embed returned nil error", i)
		}
		if !errors.Is(err, ErrDataTooLarge) {
			t.Fatalf("iteration %d: expected ErrDataTooLarge, got: %v", i, err)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Fatalf("iteration %d: failed embed left an output file behind", i)
		}
	}
}

func TestEmbedFile_RepeatableOutcome(t *testing.T) {
	tmp := t.TempDir()
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, 100, 100)

	want := deterministicBytes(3, 2_000)
	dataPath := filepath.Join(tmp, "data.bin")
	if err := os.WriteFile(dataPath, want, 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	// Repeating the same embed must consistently succeed and produce an
	// output that extracts back to the original contents.
	for i := 0; i < 3; i++ {
		out := filepath.Join(tmp, fmt.Sprintf("stego_%d.png", i))
		if err := EmbedFile(cover, dataPath, out, "pw", 1); err != nil {
			t.Fatalf("iteration %d: EmbedFile failed: %v", i, err)
		}
		got := extractAndRead(t, out, "pw", "data.bin", 1)
		if !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: round trip mismatch", i)
		}
	}
}

func TestEmbedFile_WrongPasswordFails(t *testing.T) {
	tmp := t.TempDir()
	cover := filepath.Join(tmp, "cover.png")
	writeCoverPNG(t, cover, 100, 100)

	dataPath := filepath.Join(tmp, "data.bin")
	if err := os.WriteFile(dataPath, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	out := filepath.Join(tmp, "stego.png")
	if err := EmbedFile(cover, dataPath, out, "right", 1); err != nil {
		t.Fatalf("EmbedFile failed: %v", err)
	}

	extractDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(extractDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer os.Chdir(cwd)

	if err := ExtractFile(out, "wrong", 1); err == nil {
		t.Fatalf("ExtractFile with the wrong password must fail")
	}
}
