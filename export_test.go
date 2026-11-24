package gridder

import (
	"bytes"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// newTestGridder builds a gridder with distinctive, non-default stroke widths
// so that any accidental re-stroking of the grid or border visibly changes the
// produced pixels.
func newTestGridder(t *testing.T, rows, columns int) *Gridder {
	t.Helper()
	gridder, err := New(
		ImageConfig{Width: 200, Height: 120},
		GridConfig{
			Rows:              rows,
			Columns:           columns,
			LineStrokeWidth:   2,
			BorderStrokeWidth: 6,
			LineColor:         color.NRGBA{R: 10, G: 20, B: 30, A: 255},
			BorderColor:       color.NRGBA{R: 40, G: 50, B: 60, A: 255},
		},
	)
	assert.Nil(t, err)
	return gridder
}

// failingWriter returns errWriteFailure after accepting failAfter bytes, or
// always when failAfter is zero.
type failingWriter struct {
	failAfter int
	written   int
}

var errWriteFailure = errors.New("simulated writer failure")

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, errWriteFailure
	}
	n := len(p)
	if w.written+n > w.failAfter {
		n = w.failAfter - w.written
		w.written += n
		return n, errWriteFailure
	}
	w.written += n
	return n, nil
}

// TestRepeatedExportsAreIdentical verifies that EncodePNG and SavePNG produce
// byte-for-byte identical output regardless of how many times, or in which
// order, they are called, as long as no new drawing happens in between.
func TestRepeatedExportsAreIdentical(t *testing.T) {
	gridder := newTestGridder(t, 3, 4)

	var first bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&first))
	firstBytes := append([]byte(nil), first.Bytes()...)

	tmpDir := t.TempDir()
	gridder.imageConfig.Name = filepath.Join(tmpDir, "export.png")

	for i := 0; i < 5; i++ {
		var buffer bytes.Buffer
		assert.Nil(t, gridder.EncodePNG(&buffer))
		assert.Equalf(t, firstBytes, buffer.Bytes(), "EncodePNG output changed on iteration %d", i)

		assert.Nil(t, gridder.SavePNG())
		saved, err := os.ReadFile(gridder.imageConfig.GetName())
		assert.Nil(t, err)
		assert.Equalf(t, firstBytes, saved, "SavePNG output changed on iteration %d", i)
	}

	// Grid lines and border must be rendered exactly once: the pixels must
	// match a fresh, single-export gridder carrying the same configuration.
	reference := newTestGridder(t, 3, 4)
	var referenceBuffer bytes.Buffer
	assert.Nil(t, reference.EncodePNG(&referenceBuffer))
	assert.Equal(t, referenceBuffer.Bytes(), firstBytes)
}

// TestDrawAfterExportIsReflectedWithoutReStroking verifies that content drawn
// between exports shows up in later exports while the grid and border are
// never accumulated on top of themselves.
func TestDrawAfterExportIsReflectedWithoutReStroking(t *testing.T) {
	gridder := newTestGridder(t, 3, 3)

	var before bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&before))

	assert.Nil(t, gridder.PaintCell(0, 0, color.RGBA{R: 200, G: 30, B: 30, A: 255}))
	assert.Nil(t, gridder.DrawCircle(2, 2, CircleConfig{Radius: 8, Color: color.RGBA{B: 255, A: 255}}))

	var after bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&after))
	assert.NotEqual(t, before.Bytes(), after.Bytes(), "new drawings must change the export")

	// Re-exporting must keep producing the same bytes.
	var afterAgain bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&afterAgain))
	assert.Equal(t, after.Bytes(), afterAgain.Bytes())

	// A reference gridder that performs exactly the same drawings, but only
	// exports once, must render identical pixels: any duplicated grid/border
	// strokes would make the two differ.
	reference := newTestGridder(t, 3, 3)
	assert.Nil(t, reference.PaintCell(0, 0, color.RGBA{R: 200, G: 30, B: 30, A: 255}))
	assert.Nil(t, reference.DrawCircle(2, 2, CircleConfig{Radius: 8, Color: color.RGBA{B: 255, A: 255}}))
	var referenceBuffer bytes.Buffer
	assert.Nil(t, reference.EncodePNG(&referenceBuffer))
	assert.Equal(t, referenceBuffer.Bytes(), after.Bytes())
}

// TestExportFailureRecovery verifies that failed exports leave the gridder
// fully usable: later valid exports still succeed and match the pre-failure
// content exactly.
func TestExportFailureRecovery(t *testing.T) {
	gridder := newTestGridder(t, 2, 2)
	assert.Nil(t, gridder.PaintCell(1, 1, color.Black))

	var expected bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&expected))
	expectedBytes := append([]byte(nil), expected.Bytes()...)

	// An io.Writer failing midway through the PNG must not corrupt the canvas.
	failing := &failingWriter{failAfter: 16}
	err := gridder.EncodePNG(failing)
	assert.True(t, errors.Is(err, errWriteFailure), "expected errWriteFailure, got %v", err)

	// An empty file name must be rejected without touching the canvas.
	gridder.imageConfig.Name = ""
	assert.NotNil(t, gridder.SavePNG())

	// Both failure modes followed by valid exports: content is unchanged.
	var recovered bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&recovered))
	assert.Equal(t, expectedBytes, recovered.Bytes())

	tmpDir := t.TempDir()
	gridder.imageConfig.Name = filepath.Join(tmpDir, "recovered.png")
	assert.Nil(t, gridder.SavePNG())
	saved, err := os.ReadFile(gridder.imageConfig.GetName())
	assert.Nil(t, err)
	assert.Equal(t, expectedBytes, saved)
}

// TestConcurrentExports verifies that many goroutines exporting the same
// gridder concurrently, each into its own writer, all succeed and produce
// identical, consistent output with no data races. SavePNG is exercised
// concurrently as well on equivalently configured gridders writing to
// distinct file names, since the file name lives in shared configuration.
func TestConcurrentExports(t *testing.T) {
	gridder := newTestGridder(t, 4, 4)
	assert.Nil(t, gridder.PaintCell(1, 1, color.RGBA{G: 200, A: 255}))
	assert.Nil(t, gridder.DrawCircle(3, 3))

	var expected bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&expected))
	expectedBytes := append([]byte(nil), expected.Bytes()...)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	// Same gridder, independent io.Writers: this is the concurrent export
	// path the API supports for a single instance.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			var buffer bytes.Buffer
			if err := gridder.EncodePNG(&buffer); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(expectedBytes, buffer.Bytes()) {
				errs <- errors.New("concurrent EncodePNG produced inconsistent bytes")
			}
		}()
	}

	wg.Wait()

	// SavePNG concurrently on independent gridders with distinct file names,
	// each carrying the same drawings and therefore the same expected bytes.
	tmpDir := t.TempDir()
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			saver := newTestGridder(t, 4, 4)
			if err := saver.PaintCell(1, 1, color.RGBA{G: 200, A: 255}); err != nil {
				errs <- err
				return
			}
			if err := saver.DrawCircle(3, 3); err != nil {
				errs <- err
				return
			}
			saver.imageConfig.Name = filepath.Join(tmpDir, "concurrent-"+strconv.Itoa(i)+".png")
			if err := saver.SavePNG(); err != nil {
				errs <- err
				return
			}
			saved, err := os.ReadFile(saver.imageConfig.GetName())
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(expectedBytes, saved) {
				errs <- errors.New("concurrent SavePNG produced inconsistent bytes")
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		assert.Nil(t, err)
	}
}

// applySnapshotDraw replays the first n deterministic drawing operations used
// by TestConcurrentExportsAndDraws. Both the gridder under test and the
// reference snapshot builders must use exactly this sequence.
func applySnapshotDraw(g *Gridder, index int) {
	row := index / 5
	column := index % 5
	fillColor := color.RGBA{R: uint8(20 + index*3), G: uint8(40 + index*2), B: uint8(60 + index), A: 255}
	if index%2 == 0 {
		_ = g.PaintCell(row, column, fillColor)
	} else {
		_ = g.DrawCircle(row, column, CircleConfig{
			Radius: 7,
			Color:  fillColor,
		})
	}
}

// TestConcurrentExportsAndDraws interleaves concurrent drawing and exporting
// and verifies that every export observes a complete, consistent snapshot of
// the canvas (the state after some whole drawing operation) rather than a
// half-updated state. Run under -race this also proves the locking discipline.
//
// Drawings are applied through a single worker goroutine in a fixed order so
// the set of reachable quiescent states is well defined, while exports run on
// many goroutines concurrently with that worker. An atomic counter records how
// many drawings have completed; each export captures the counter together with
// its bytes and must match the reference snapshot for that exact prefix.
func TestConcurrentExportsAndDraws(t *testing.T) {
	const totalDraws = 20

	gridder := newTestGridder(t, 4, 5)

	// Reference snapshots for every quiescent prefix of the draw sequence.
	// Each reference gridder exports exactly once and so cannot re-stroke.
	references := make([][]byte, totalDraws+1)
	for n := 0; n <= totalDraws; n++ {
		reference := newTestGridder(t, 4, 5)
		for i := 0; i < n; i++ {
			applySnapshotDraw(reference, i)
		}
		var buffer bytes.Buffer
		assert.Nil(t, reference.EncodePNG(&buffer))
		references[n] = append([]byte(nil), buffer.Bytes()...)
	}

	var completed int64
	drawJobs := make(chan int)
	var drawWG sync.WaitGroup
	drawWG.Add(1)
	go func() {
		defer drawWG.Done()
		for index := range drawJobs {
			applySnapshotDraw(gridder, index)
			atomic.AddInt64(&completed, 1)
		}
	}()

	type observation struct {
		done   int64
		pixels []byte
	}
	var exportMu sync.Mutex
	observed := make([]observation, 0, 200)
	var exportWG sync.WaitGroup

	for i := 0; i < 16; i++ {
		exportWG.Add(1)
		go func() {
			defer exportWG.Done()
			for j := 0; j < 10; j++ {
				pixels, done := exportAndCount(gridder, &completed)
				exportMu.Lock()
				observed = append(observed, observation{
					done:   done,
					pixels: pixels,
				})
				exportMu.Unlock()
			}
		}()
	}

	// Feed the draw jobs while exporters are running.
	for i := 0; i < totalDraws; i++ {
		drawJobs <- i
	}
	close(drawJobs)
	drawWG.Wait()
	exportWG.Wait()

	assert.NotEmpty(t, observed)
	for index, entry := range observed {
		if entry.done < 0 || entry.done > totalDraws {
			t.Fatalf("export %d recorded an impossible completion count %d", index, entry.done)
		}
		if !bytes.Equal(references[entry.done], entry.pixels) {
			t.Fatalf("export %d observed a canvas inconsistent with the %d completed drawing(s): torn or mixed state", index, entry.done)
		}
	}

	// Once all drawings have settled, exports must equal the final snapshot
	// and remain stable across repeated exports.
	var final bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&final))
	assert.Equal(t, references[totalDraws], final.Bytes())

	var finalAgain bytes.Buffer
	assert.Nil(t, gridder.EncodePNG(&finalAgain))
	assert.Equal(t, final.Bytes(), finalAgain.Bytes())
}
