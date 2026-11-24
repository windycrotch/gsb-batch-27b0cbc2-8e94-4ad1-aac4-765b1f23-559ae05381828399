// The MIT License (MIT)
//
// Copyright (c) 2019 Amangeldy Kadyl
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package vp8

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kolesa-team/go-webp/v2/internal/pipeline"
	"github.com/kolesa-team/go-webp/v2/internal/status"
	"github.com/kolesa-team/go-webp/v2/internal/workers"
	"github.com/kolesa-team/go-webp/v2/utils"
)

// stopSink asks the decoder to stop once stopAt (or more) output rows have
// been committed, or rejects at Buffer after bufferAfter Buffer calls (0 =
// never). A VP8 decoder finalizes rows one macroblock row (up to 16 output
// rows) at a time, so a Commit always ends a batch: the stop takes effect at
// the first batch boundary at or past stopAt. It records every call and
// keeps the output pre-filled with a marker so any write past the committed
// rows is detectable.
type stopSink struct {
	img *image.NRGBA

	stopAt      int  // stop at the first Commit whose end reaches this row
	bufferAfter int  // Buffer call number (1-based) that returns ErrStop; 0 = never
	wrapStop    bool // wrap the sentinel instead of returning it directly

	bufferCalls int
	commitCalls int
	committed   int // rows successfully committed
	next        int // next expected source row
	pending     [2]int
	hasPending  bool
	stopped     bool
}

type wrappedStop struct{ err error }

func (e *wrappedStop) Error() string { return "wrapped: " + e.err.Error() }
func (e *wrappedStop) Unwrap() error { return e.err }

func (s *stopSink) stopErr() error {
	if s.wrapStop {
		return &wrappedStop{err: pipeline.ErrStop}
	}
	return pipeline.ErrStop
}

func (s *stopSink) Buffer(y, n int) ([]byte, error) {
	s.bufferCalls++
	if s.bufferAfter != 0 && s.bufferCalls == s.bufferAfter {
		// Refuse before a single pixel of this batch is handed out.
		return nil, s.stopErr()
	}
	if y != s.next || n <= 0 || y+n > s.img.Rect.Dy() {
		return nil, fmt.Errorf("Buffer(%d, %d), want start %d", y, n, s.next)
	}
	s.pending, s.hasPending = [2]int{y, n}, true
	return s.img.Pix[y*s.img.Stride : (y+n)*s.img.Stride], nil
}

func (s *stopSink) Stride() int { return s.img.Stride }

func (s *stopSink) Commit(y, n int) error {
	if !s.hasPending || [2]int{y, n} != s.pending {
		return fmt.Errorf("Commit(%d, %d) without a matching Buffer", y, n)
	}
	s.hasPending = false
	s.next = y + n
	s.committed = y + n
	s.commitCalls++
	if s.stopAt > 0 && s.committed >= s.stopAt && !s.stopped {
		s.stopped = true
		return s.stopErr()
	}
	return nil
}

func (s *stopSink) NeedRows() int { return s.img.Rect.Dy() }

// markerSink returns a w x h image pre-filled with the given marker and the
// sink that writes into it.
func markerSink(w, h, marker int, stopAt, bufferAfter int) (*image.NRGBA, *stopSink) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(marker)
	}
	return img, &stopSink{img: img, stopAt: stopAt, bufferAfter: bufferAfter}
}

func prefixBytes(img *image.NRGBA, n int) []byte { return img.Pix[:n*img.Stride] }

// allMarker reports whether b consists solely of the marker byte.
func allMarker(b []byte, marker byte) bool {
	for _, x := range b {
		if x != marker {
			return false
		}
	}
	return true
}

// lossyScenes are the full-frame and cropped geometries used across the
// stop tests.
var lossyScenes = []struct {
	name string
	crop *image.Rectangle
	w, h int
}{
	{"full", nil, 250, 199},
	{"crop", ptr(image.Rect(37, 50, 201, 180)), 164, 130},
}

func ptr(r image.Rectangle) *image.Rectangle { return &r }

// decodeScene runs Decode or DecodeRegion into a fresh image.
func decodeScene(t *testing.T, vp8 []byte, opts *Options, sc struct {
	name string
	crop *image.Rectangle
	w, h int
}) *image.NRGBA {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, sc.w, sc.h))
	if sc.crop != nil {
		if err := DecodeRegion(vp8, nil, opts, *sc.crop, pipeline.NewDirect(img)); err != nil {
			t.Fatalf("%s reference: %v", sc.name, err)
		}
	} else if err := Decode(vp8, nil, opts, pipeline.NewDirect(img)); err != nil {
		t.Fatalf("%s reference: %v", sc.name, err)
	}
	return img
}

func runScene(vp8 []byte, opts *Options, sc struct {
	name string
	crop *image.Rectangle
	w, h int
}, s pipeline.Sink) error {
	if sc.crop != nil {
		return DecodeRegion(vp8, nil, opts, *sc.crop, s)
	}
	return Decode(vp8, nil, opts, s)
}

// TestStopAtCommit covers the main contract for Decode and DecodeRegion:
// after Commit returns the stop signal the decode ends with nil error, the
// committed rows are byte-identical to a full decode, and no row past the
// stop point was written. Sequential and threaded, fancy and point
// upsampling, whole frame and a crop.
func TestStopAtCommit(t *testing.T) {
	vp8 := loadVP8(t, "mix_250x199_o4.webp")
	// Stop requests at and around macroblock-row boundaries (rows are
	// finalized in batches of up to 16; the first batch is shorter).
	stopAts := []int{1, 7, 8, 16, 17, 32, 33, 64, 100, 130, 199}
	for _, sc := range lossyScenes {
		var optsList []Options
		for _, threads := range []bool{false, true} {
			optsList = append(optsList,
				Options{UseThreads: threads},
				Options{UseThreads: threads, NoFancyUpsampling: true},
				Options{UseThreads: threads, BypassFiltering: true},
			)
		}
		for _, opts := range optsList {
			ref := decodeScene(t, vp8, &opts, sc)
			for _, stopAt := range stopAts {
				if stopAt > sc.h {
					continue
				}
				for _, wrap := range []bool{false, true} {
					img, s := markerSink(sc.w, sc.h, 0x77, stopAt, 0)
					s.wrapStop = wrap
					label := fmt.Sprintf("%s opts=%+v stop=%d wrap=%v", sc.name, opts, stopAt, wrap)
					if err := runScene(vp8, &opts, sc, s); err != nil {
						t.Fatalf("%s: Decode returned %v, want nil after the stop signal", label, err)
					}
					if s.commitCalls == 0 {
						t.Fatalf("%s: no Commit calls", label)
					}
					if s.committed < stopAt {
						t.Fatalf("%s: only %d rows committed, want at least %d", label, s.committed, stopAt)
					}
					if s.hasPending {
						t.Fatalf("%s: a Buffer batch was left uncommitted", label)
					}
					// Committed prefix matches the full decode.
					if got, want := prefixBytes(s.img, s.committed), prefixBytes(ref, s.committed); !bytes.Equal(got, want) {
						t.Fatalf("%s: committed rows differ from the full decode", label)
					}
					// Everything below the committed rows is still the marker.
					if tail := img.Pix[s.committed*img.Stride:]; !allMarker(tail, 0x77) {
						t.Fatalf("%s: uncommitted rows were written", label)
					}
					// Stopping at the final row must leave the complete image.
					if stopAt == sc.h && (!bytes.Equal(img.Pix, ref.Pix) || s.committed != sc.h) {
						t.Fatalf("%s: stop at the last row did not produce the full image", label)
					}
				}
			}
		}
	}
}

// TestStopAtBuffer checks that a sink rejecting at Buffer stops the decode
// cleanly (nil, ErrStop honored): the rejected batch is never committed and
// none of its pixels are written. Batches committed before the rejection
// stay final (they were already complete); a rejection on the very first
// Buffer call leaves the whole image untouched.
func TestStopAtBuffer(t *testing.T) {
	vp8 := loadVP8(t, "mix_250x199_o4.webp")
	for _, threads := range []bool{false, true} {
		for _, sc := range lossyScenes {
			for _, batch := range []int{1, 2, 5} {
				opts := &Options{UseThreads: threads}
				img, s := markerSink(sc.w, sc.h, 0x33, 0, batch)
				label := fmt.Sprintf("%s threads=%v batch=%d", sc.name, threads, batch)
				if err := runScene(vp8, opts, sc, s); err != nil {
					t.Fatalf("%s: %v, want nil after a Buffer stop", label, err)
				}
				if s.hasPending {
					t.Fatalf("%s: rejected batch left pending state", label)
				}
				// The rejected Buffer call is never followed by a Commit:
				// exactly the earlier batches (batch-1 Buffer calls) were
				// committed.
				if s.commitCalls != batch-1 {
					t.Fatalf("%s: %d Commit calls, want %d (the rejected batch is not committed)", label, s.commitCalls, batch-1)
				}
				if s.bufferCalls != batch {
					t.Fatalf("%s: %d Buffer calls, want exactly %d", label, s.bufferCalls, batch)
				}
				// The already committed prefix is intact; every byte below it
				// is still the marker (the rejected batch wrote nothing).
				if s.committed > 0 {
					if got, want := prefixBytes(img, s.committed), prefixBytes(decodeScene(t, vp8, opts, sc), s.committed); !bytes.Equal(got, want) {
						t.Fatalf("%s: previously committed rows changed", label)
					}
				}
				if tail := img.Pix[s.committed*img.Stride:]; !allMarker(tail, 0x33) {
					t.Fatalf("%s: pixels of the rejected batch were written", label)
				}
			}
		}
	}
}

// TestStopVsTruncation makes the distinction explicit: a sink stop returns
// nil when the decoder stops before the corrupt region, while the same
// truncated stream, decoded past that point, fails with
// VP8_STATUS_NOT_ENOUGH_DATA. Neither path reports half-written rows as a
// success.
func TestStopVsTruncation(t *testing.T) {
	full := loadVP8(t, "mix_250x199_o4.webp")
	// Cut inside the token partitions but far enough in for several whole
	// macroblock rows to parse and finalize first (validated below).
	cut := len(full) * 3 / 10
	trunc := full[:cut]
	// Sanity: the truncated stream is a NotEnoughData error when fully
	// decoded, and it did finalize some rows first.
	probe := &batchProbe{img: image.NewNRGBA(image.Rect(0, 0, 250, 199))}
	err := Decode(trunc, nil, nil, probe)
	if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
		t.Fatalf("full truncated decode: code %v (%v), want NotEnoughData", code, err)
	}
	if probe.committed == 0 {
		t.Fatal("truncated stream finalizes no rows; pick a later cut")
	}
	for _, threads := range []bool{false, true} {
		opts := &Options{UseThreads: threads}
		safeStop := probe.committed // the last good batch ends here
		img, s := markerSink(250, 199, 0x44, safeStop, 0)
		if err := Decode(trunc, nil, opts, s); err != nil {
			t.Fatalf("threads=%v: early stop on truncated data returned %v, want nil", threads, err)
		}
		if s.committed != safeStop {
			t.Fatalf("threads=%v: %d committed rows, want %d", threads, s.committed, safeStop)
		}
		// Those rows must equal the same prefix of the intact stream.
		goodImg, good := markerSink(250, 199, 0x44, safeStop, 0)
		if err := Decode(full, nil, opts, good); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(prefixBytes(img, safeStop), prefixBytes(goodImg, safeStop)) {
			t.Fatalf("threads=%v: rows stopped before truncation differ from the intact stream", threads)
		}
		// Asking for the whole frame still surfaces the corruption.
		far := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: 199}
		err := Decode(trunc, nil, opts, far)
		if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
			t.Fatalf("threads=%v: code %v (%v), want NotEnoughData", threads, code, err)
		}
	}
}

// TestStopThreadedParserReadahead is a regression test for the UseThreads
// interaction between a clean sink stop and truncation. The parser runs up
// to two batches ahead of the reconstruction/emit stage, so on a truncated
// stream it can hit the premature end before it observes the sink's stop.
// A sink signal the pipeline actually reaches must win (nil); a parse error
// encountered before the sink can signal still fails (the same precedence
// context cancellation already has). Every cut and every emit-helper policy
// is swept because the race depends on where the cut falls.
func TestStopThreadedParserReadahead(t *testing.T) {
	checkNoLeak(t)
	full := loadVP8(t, "mix_250x199_o4.webp")
	setBatch(t, 1) // maximize the distance the parser can run ahead
	cutsThatEmit := 0
	for _, em := range emitModes {
		setEmit(t, em.waits)
		for pct := 10; pct <= 75; pct++ {
			cut := len(full) * pct / 100
			// Sequential reference: where does the truncated stream stop?
			probe := &batchProbe{img: image.NewNRGBA(image.Rect(0, 0, 250, 199))}
			serr := Decode(full[:cut], nil, &Options{}, probe)
			if serr == nil || probe.committed == 0 {
				continue // cut before the first emitted row, or still complete
			}
			if code := status.CodeOf(serr); code != utils.Vp8StatusNotEnoughData {
				continue // a different corruption kind, not the target case
			}
			cutsThatEmit++
			// Threaded Commit stop at exactly the last good row: must be nil.
			s := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: probe.committed}
			if err := Decode(full[:cut], nil, &Options{UseThreads: true}, s); err != nil {
				t.Fatalf("em=%s cut=%d%%: clean Commit stop returned %v, want nil (parser read ahead into the cut)", em.name, pct, err)
			}
			if s.committed != probe.committed {
				t.Fatalf("em=%s cut=%d%%: committed %d rows, want %d", em.name, pct, s.committed, probe.committed)
			}
			// Threaded Buffer rejection at the last Buffer call a sequential
			// run actually reaches (proven reachable, so the signal wins).
			cnt := &countBufSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199))}
			if err := Decode(full[:cut], nil, &Options{}, cnt); err != nil && cnt.buffers == 0 {
				t.Fatalf("em=%s cut=%d%%: sequential reference made no Buffer call", em.name, pct)
			}
			bs := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), bufferAfter: max(1, cnt.buffers)}
			if err := Decode(full[:cut], nil, &Options{UseThreads: true}, bs); err != nil {
				t.Fatalf("em=%s cut=%d%%: clean Buffer stop returned %v, want nil", em.name, pct, err)
			}
			if bs.commitCalls >= bs.bufferCalls {
				t.Fatalf("em=%s cut=%d%%: rejected Buffer was followed by a Commit", em.name, pct)
			}
			// Threaded full decode of the same cut still fails.
			far := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: 199}
			if err := Decode(full[:cut], nil, &Options{UseThreads: true}, far); err == nil {
				t.Fatalf("em=%s cut=%d%%: truncated full decode succeeded", em.name, pct)
			} else if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
				t.Fatalf("em=%s cut=%d%%: code %v, want NotEnoughData", em.name, pct, code)
			}
		}
	}
	if cutsThatEmit == 0 {
		t.Fatal("no truncated cut emitted rows before failing; the sweep found nothing to test")
	}
}

// countBufSink counts how many Buffer calls a (truncated) decode makes.
type countBufSink struct {
	img     *image.NRGBA
	buffers int
}

func (s *countBufSink) Buffer(y, n int) ([]byte, error) {
	s.buffers++
	return s.img.Pix[y*s.img.Stride : (y+n)*s.img.Stride], nil
}
func (s *countBufSink) Stride() int           { return s.img.Stride }
func (s *countBufSink) Commit(y, n int) error { return nil }
func (s *countBufSink) NeedRows() int         { return s.img.Rect.Dy() }

// batchProbe records how many rows a (truncated) decode commits.
type batchProbe struct {
	img       *image.NRGBA
	committed int
}

func (p *batchProbe) Buffer(y, n int) ([]byte, error) {
	return p.img.Pix[y*p.img.Stride : (y+n)*p.img.Stride], nil
}
func (p *batchProbe) Stride() int           { return p.img.Stride }
func (p *batchProbe) Commit(y, n int) error { p.committed = y + n; return nil }
func (p *batchProbe) NeedRows() int         { return p.img.Rect.Dy() }

// TestStopThreadCleanup verifies the UseThreads requirement directly: after
// many decodes stopped halfway, both pipelines have exited (no goroutine
// reads input or writes caches anymore) and the worker budget is back to its
// initial availability. It covers every emit-helper policy.
func TestStopThreadCleanup(t *testing.T) {
	checkNoLeak(t)
	setBatch(t, 1) // small batches: the stop lands mid-pipeline
	vp8 := loadVP8(t, "grad_250x199_o9.webp")
	for _, em := range emitModes {
		setEmit(t, em.waits)
		limit := workers.Limit()
		before := workers.InUse()
		for round := 0; round < 25; round++ {
			stopAt := 1 + (round*37)%150
			s := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: stopAt}
			if err := Decode(vp8, nil, &Options{UseThreads: true}, s); err != nil {
				t.Fatalf("em=%s round=%d: %v", em.name, round, err)
			}
			if s.committed < stopAt {
				t.Fatalf("em=%s round=%d: committed %d rows, want at least %d", em.name, round, s.committed, stopAt)
			}
			if got := workers.InUse(); got != before {
				t.Fatalf("em=%s round=%d: workers.InUse()=%d, want %d (budget leaked)", em.name, round, got, before)
			}
			if got := workers.HandOffs(); got != 0 {
				t.Fatalf("em=%s round=%d: %d pipelines still registered", em.name, round, got)
			}
		}
		if got := workers.Limit(); got != limit {
			t.Fatalf("em=%s: worker budget changed: %d, want %d", em.name, got, limit)
		}
	}
	// A runtime goroutine count check in addition to the bookkeeping ones.
	base := runtime.NumGoroutine()
	s := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: 3}
	if err := Decode(vp8, nil, &Options{UseThreads: true}, s); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > base {
		buf := make([]byte, 1<<16)
		t.Fatalf("%d goroutines remain after the stopped decode:\n%s", got-base, buf[:runtime.Stack(buf, true)])
	}
}

// TestStopAfterBufferUnderBudget exercises the threaded path with the
// emitter active and a Buffer rejection, which must take down both the
// reconstruction and emit helpers without deadlocking.
func TestStopAfterBufferUnderBudget(t *testing.T) {
	checkNoLeak(t)
	setBatch(t, 1)
	setBudget(t, 2, false)
	setEmit(t, 0) // emitter starts with the first batch
	vp8 := loadVP8(t, "grad_250x199_o9.webp")
	done := make(chan error, 1)
	go func() {
		_, s := markerSink(250, 199, 0x55, 0, 3)
		done <- Decode(vp8, nil, &Options{UseThreads: true}, s)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Decode: %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Decode did not return after a Buffer stop with two helpers")
	}
	if n := workers.InUse(); n != 0 {
		t.Fatalf("%d helper slots still in use", n)
	}
}

// TestStopThenReuse runs several stop/full decode pairs and checks the full
// output is identical to a decode that never stopped, sequentially and
// threaded. The pooled decoder state must not retain the early stop.
func TestStopThenReuse(t *testing.T) {
	vp8 := loadVP8(t, "mix_250x199_o4.webp")
	for _, threads := range []bool{false, true} {
		opts := &Options{UseThreads: threads}
		want, err := decodeImage(vp8, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		for round := 0; round < 5; round++ {
			stop := &stopSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199)), stopAt: 7 + round*31}
			if err := Decode(vp8, nil, opts, stop); err != nil {
				t.Fatalf("threads=%v round=%d stop: %v", threads, round, err)
			}
			full := image.NewNRGBA(image.Rect(0, 0, 250, 199))
			if err := Decode(vp8, nil, opts, pipeline.NewDirect(full)); err != nil {
				t.Fatalf("threads=%v round=%d full: %v", threads, round, err)
			}
			if !bytes.Equal(full.Pix, want.Pix) {
				t.Fatalf("threads=%v round=%d: full decode after a stop differs", threads, round)
			}
		}
	}
}

// TestStopCropOutputRows proves the stop point is counted in output rows of
// the cropped region, not frame rows: the sink only sees rows numbered from
// 0 within the crop, and stopping there commits exactly that prefix.
func TestStopCropOutputRows(t *testing.T) {
	vp8 := loadVP8(t, "mix_250x199_o4.webp")
	// Even origin (libwebp snaps lossy crops to even coordinates).
	crop := image.Rect(40, 52, 210, 170)
	for _, threads := range []bool{false, true} {
		opts := &Options{UseThreads: threads}
		ref := image.NewNRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
		if err := DecodeRegion(vp8, nil, opts, crop, pipeline.NewDirect(ref)); err != nil {
			t.Fatal(err)
		}
		for _, stopAt := range []int{1, 16, 33} {
			img, s := markerSink(crop.Dx(), crop.Dy(), 0x66, stopAt, 0)
			if err := DecodeRegion(vp8, nil, opts, crop, s); err != nil {
				t.Fatalf("threads=%v stop=%d: %v", threads, stopAt, err)
			}
			if s.committed < stopAt {
				t.Fatalf("threads=%v stop=%d: committed %d output rows, want at least %d", threads, stopAt, s.committed, stopAt)
			}
			// Every committed row the sink saw was numbered inside the crop.
			if s.next > crop.Dy() {
				t.Fatalf("threads=%v: sink received row %d beyond the crop height %d", threads, s.next, crop.Dy())
			}
			if !bytes.Equal(prefixBytes(img, s.committed), prefixBytes(ref, s.committed)) {
				t.Fatalf("threads=%v stop=%d: cropped output rows differ", threads, stopAt)
			}
		}
	}
}

// TestStopIsSentinel checks the sentinel identity and the errors.Is chain
// used by callers to recognize the signal.
func TestStopIsSentinel(t *testing.T) {
	if !errors.Is(pipeline.ErrStop, pipeline.ErrStop) {
		t.Fatal("ErrStop does not match itself")
	}
	wrapped := fmt.Errorf("custom: %w", pipeline.ErrStop)
	if !errors.Is(wrapped, pipeline.ErrStop) {
		t.Fatal("errors.Is fails on a wrapped ErrStop")
	}
	// The stop signal carries no libwebp status code (CodeOf only tags
	// *status.Error values), which is how a stop is told from a failure.
	var se *status.Error
	if errors.As(pipeline.ErrStop, &se) {
		t.Fatalf("ErrStop carries a status code %v", se.Code)
	}
	for _, code := range []utils.VP8StatusCode{
		utils.Vp8StatusNotEnoughData,
		utils.Vp8StatusBitstreamError,
		utils.Vp8StatusUserAbort,
	} {
		if errors.As(pipeline.ErrStop, &se) && se.Code == code {
			t.Fatalf("ErrStop reports status %v", code)
		}
	}
}

// stopAtomicSink stops from Commit using an atomic flag toggled by another
// goroutine, to exercise the threaded pipeline under an asynchronous stop.
type stopAtomicSink struct {
	img  *image.NRGBA
	stop atomic.Bool
	rows atomic.Int64
}

func (s *stopAtomicSink) Buffer(y, n int) ([]byte, error) {
	return s.img.Pix[y*s.img.Stride : (y+n)*s.img.Stride], nil
}
func (s *stopAtomicSink) Stride() int { return s.img.Stride }
func (s *stopAtomicSink) Commit(y, n int) error {
	s.rows.Store(int64(y + n))
	if s.stop.Load() {
		return pipeline.ErrStop
	}
	return nil
}
func (s *stopAtomicSink) NeedRows() int { return s.img.Rect.Dy() }

// TestStopAsyncThreaded sets the stop flag while the threaded decode is in
// flight and requires a clean return with both helpers joined.
func TestStopAsyncThreaded(t *testing.T) {
	checkNoLeak(t)
	setBatch(t, 1)
	setBudget(t, 2, false)
	vp8 := loadVP8(t, "grad_250x199_o9.webp")
	for _, em := range emitModes {
		setEmit(t, em.waits)
		s := &stopAtomicSink{img: image.NewNRGBA(image.Rect(0, 0, 250, 199))}
		done := make(chan error, 1)
		go func() { done <- Decode(vp8, nil, &Options{UseThreads: true}, s) }()
		// Toggle the stop after a few rows are out; the decode reaches it at
		// the next batch boundary.
		waitDeadline := time.Now().Add(5 * time.Second)
		for s.rows.Load() < 3 && time.Now().Before(waitDeadline) {
			runtime.Gosched()
		}
		if s.rows.Load() < 3 {
			t.Fatalf("em=%s: decode produced no rows before the grace period ended", em.name)
		}
		s.stop.Store(true)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("em=%s: %v, want nil", em.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("em=%s: decode hung after the asynchronous stop", em.name)
		}
		if s.rows.Load() == 0 {
			t.Fatalf("em=%s: no rows committed before the stop", em.name)
		}
		if n := workers.InUse(); n != 0 {
			t.Fatalf("em=%s: %d helpers still running", em.name, n)
		}
	}
}
