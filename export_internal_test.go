package gridder

import (
	"bytes"
	"sync/atomic"

	"github.com/fogleman/gg"
)

// exportAndCount is a test-only helper that atomically pairs an export with
// the value of an external completion counter. It acquires the same read lock
// used internally by EncodePNG/SavePNG so the observed counter and the
// rendered snapshot correspond to one and the same locked canvas state.
//
// The counter is supplied by the test and incremented by drawing goroutines
// after each drawing operation returns.
func exportAndCount(g *Gridder, counter *int64) ([]byte, int64) {
	g.mu.RLock()
	snapshot := gg.NewContextForImage(g.ctx.Image())
	done := atomic.LoadInt64(counter)
	g.mu.RUnlock()

	margin := float64(g.gridConfig.GetMarginWidth())
	snapshot.Translate(margin, margin)
	g.paintGrid(snapshot)
	g.paintBorder(snapshot)

	var buffer bytes.Buffer
	_ = snapshot.EncodePNG(&buffer)
	return buffer.Bytes(), done
}
