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

package decoder

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/kolesa-team/go-webp/v2/internal/container"
	"github.com/kolesa-team/go-webp/v2/internal/pipeline"
	"github.com/kolesa-team/go-webp/v2/internal/status"
	"github.com/kolesa-team/go-webp/v2/internal/vp8"
	"github.com/kolesa-team/go-webp/v2/internal/workers"
	"github.com/kolesa-team/go-webp/v2/utils"
)

// rowStopSink drives the internal lossy row interface: it stops the decode
// at the first Commit whose end reaches stopAt rows (0 = run to completion),
// or rejects the bufferAfter-th Buffer call. The destination is pre-filled
// with a marker, so writes past the committed rows are detectable.
type rowStopSink struct {
	img         *image.NRGBA
	stopAt      int
	bufferAfter int
	committed   int
	buffers     int
	commits     int
	pending     [2]int
	hasPending  bool
	next        int
}

func (s *rowStopSink) Buffer(y, n int) ([]byte, error) {
	s.buffers++
	if s.bufferAfter != 0 && s.buffers == s.bufferAfter {
		return nil, pipeline.ErrStop
	}
	if y != s.next {
		return nil, fmt.Errorf("Buffer(%d, %d): expected row %d", y, n, s.next)
	}
	s.pending, s.hasPending = [2]int{y, n}, true
	return s.img.Pix[y*s.img.Stride : (y+n)*s.img.Stride], nil
}

func (s *rowStopSink) Stride() int { return s.img.Stride }

func (s *rowStopSink) Commit(y, n int) error {
	if !s.hasPending || s.pending != [2]int{y, n} {
		return fmt.Errorf("Commit(%d, %d) without a matching Buffer", y, n)
	}
	s.hasPending = false
	s.next = y + n
	s.committed = y + n
	s.commits++
	if s.stopAt != 0 && s.committed >= s.stopAt {
		return pipeline.ErrStop
	}
	return nil
}

func (s *rowStopSink) NeedRows() int { return s.img.Rect.Dy() }

func newMarkedSink(w, h, stopAt, bufferAfter int) (*image.NRGBA, *rowStopSink) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x6c
	}
	return img, &rowStopSink{img: img, stopAt: stopAt, bufferAfter: bufferAfter}
}

func allByte(b []byte, want byte) bool {
	for _, x := range b {
		if x != want {
			return false
		}
	}
	return true
}

// decodeLossyRows parses data and runs the lossy row interface into sink,
// wiring the lazy alpha plane exactly like Decoder.decodeLossy does.
func decodeLossyRows(data []byte, useThreads bool, sink pipeline.Sink) error {
	parsed, err := container.Parse(data)
	if err != nil {
		return err
	}
	opts := &vp8.Options{UseThreads: useThreads}
	if parsed.ALPH != nil {
		w, h := parsed.Width, parsed.Height
		la := newLazyAlpha(parsed.ALPH, w, h, image.Rect(0, 0, w, h), &Config{UseThreads: useThreads})
		defer la.release()
		opts.Alpha = la.get
	}
	return vp8.Decode(parsed.VP8, nil, opts, sink)
}

// TestLossyRowStop runs the full decoder machinery (container parsing, lazy
// alpha and the row sink) but stops at the row interface, and checks the
// committed prefix equals the corresponding prefix of a normal Decode, for
// opaque and alpha lossy pictures, sequentially and threaded.
func TestLossyRowStop(t *testing.T) {
	files := []string{"m4_q75.webp", "100x150_lossy.webp", "testdata/bigvalue_flip.webp"}
	for _, file := range files {
		data, err := os.ReadFile(testDataPath(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		parsed, err := container.Parse(data)
		if err != nil {
			t.Fatalf("%s: parse: %v", file, err)
		}
		if parsed.VP8 == nil {
			continue
		}
		w, h := parsed.Width, parsed.Height
		full := decode(t, data, nil)
		for _, threads := range []bool{false, true} {
			for _, stopAt := range []int{1, max(1, h/4), max(1, h/2), h} {
				img, s := newMarkedSink(w, h, stopAt, 0)
				label := fmt.Sprintf("%s threads=%v stop=%d", file, threads, stopAt)
				if err := decodeLossyRows(data, threads, s); err != nil {
					t.Fatalf("%s: %v, want nil after stop", label, err)
				}
				if s.committed < stopAt || s.hasPending {
					t.Fatalf("%s: committed=%d pending=%v", label, s.committed, s.hasPending)
				}
				if got, want := img.Pix[:s.committed*img.Stride], full.Pix[:s.committed*full.Stride]; !bytes.Equal(got, want) {
					t.Fatalf("%s: committed prefix differs from a full Decode", label)
				}
				if tail := img.Pix[s.committed*img.Stride:]; !allByte(tail, 0x6c) {
					t.Fatalf("%s: uncommitted rows were written", label)
				}
			}
		}
	}
}

// testDataPath maps a test-relative name to its path: names under
// testdata/ already are; shared images live in ../test_data/images.
func testDataPath(name string) string {
	if len(name) >= 9 && name[:9] == "testdata/" {
		return name
	}
	return "../test_data/images/" + name
}

// TestLossyRowStopBufferReject checks that a Buffer rejection at the row
// interface ends the decode with nil and no Commit for the rejected batch,
// with no pixels of that batch written, including under UseThreads.
func TestLossyRowStopBufferReject(t *testing.T) {
	data := readFile(t, "100x150_lossy.webp")
	parsed, err := container.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, threads := range []bool{false, true} {
		for _, batch := range []int{1, 2, 4} {
			img, s := newMarkedSink(parsed.Width, parsed.Height, 0, batch)
			label := fmt.Sprintf("threads=%v batch=%d", threads, batch)
			err := vp8.Decode(parsed.VP8, nil, &vp8.Options{UseThreads: threads}, s)
			if err != nil {
				t.Fatalf("%s: %v, want nil", label, err)
			}
			if s.commits != batch-1 {
				t.Fatalf("%s: %d commits, want %d", label, s.commits, batch-1)
			}
			if s.hasPending {
				t.Fatalf("%s: rejected batch pending", label)
			}
			if tail := img.Pix[s.committed*img.Stride:]; !allByte(tail, 0x6c) {
				t.Fatalf("%s: rejected batch wrote pixels", label)
			}
		}
	}
}

// TestDecoderReuseAfterStop is the headline reuse requirement: stop a
// threaded decode halfway, then reuse the same *Decoder for a complete
// Decode of the same picture and compare against the first, never-stopped
// complete decode. Repeated many times to also prove the worker budget
// returns to its initial availability each time.
func TestDecoderReuseAfterStop(t *testing.T) {
	g := runtime.NumGoroutine()
	data := readFile(t, "m4_q75.webp")
	parsed, err := container.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := decode(t, data, &Options{UseThreads: true})
	dec := newDec(t, data, &Options{UseThreads: true})
	for round := 0; round < 10; round++ {
		// Halfway stop through the row interface (mirrors what a progressive
		// preview sink does), threaded.
		_, s := newMarkedSink(parsed.Width, parsed.Height, parsed.Height/2+1, 0)
		if err := vp8.Decode(parsed.VP8, nil, &vp8.Options{UseThreads: true}, s); err != nil {
			t.Fatalf("round %d stop: %v", round, err)
		}
		if n := workers.InUse(); n != 0 {
			t.Fatalf("round %d: %d worker slots still taken after the stop", round, n)
		}
		if n := workers.HandOffs(); n != 0 {
			t.Fatalf("round %d: %d pipelines still registered", round, n)
		}
		// Reuse the same decoder instance for a full decode.
		got, err := dec.Decode()
		if err != nil {
			t.Fatalf("round %d reuse Decode: %v", round, err)
		}
		if !bytes.Equal(got.(*image.NRGBA).Pix, want.Pix) {
			t.Fatalf("round %d: reused decoder output differs from the first full decode", round)
		}
	}
	waitGoroutines(t, g)
}

// TestDecoderReuseAfterBufferStop covers reuse after a Buffer-stage
// rejection rather than a Commit stop.
func TestDecoderReuseAfterBufferStop(t *testing.T) {
	data := readFile(t, "100x150_lossy.webp")
	parsed, err := container.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := decode(t, data, nil)
	dec := newDec(t, data, nil)
	for round := 0; round < 5; round++ {
		_, s := newMarkedSink(parsed.Width, parsed.Height, 0, 1+round%3)
		if err := vp8.Decode(parsed.VP8, nil, &vp8.Options{UseThreads: true}, s); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		got, err := dec.Decode()
		if err != nil {
			t.Fatalf("round %d reuse: %v", round, err)
		}
		if !bytes.Equal(got.(*image.NRGBA).Pix, want.Pix) {
			t.Fatalf("round %d: reused decoder output differs", round)
		}
	}
}

// TestStopVsNotEnoughDataAtDecoder distinguishes an active stop from
// corruption: a VP8 partition that ends early still fails with
// VP8_STATUS_NOT_ENOUGH_DATA, while a stop before the truncation returns
// nil; half-written rows are never reported as success.
func TestStopVsNotEnoughDataAtDecoder(t *testing.T) {
	data := readFile(t, "m4_q75.webp")
	parsed, err := container.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the VP8 payload inside the token partitions but past several whole
	// macroblock rows: the container stays valid and the decoder reaches the
	// premature end after emitting rows.
	truncVP8 := parsed.VP8[:len(parsed.VP8)*3/10]
	// How many rows does the truncated stream finalize before the error?
	probe := &rowStopSink{img: image.NewNRGBA(image.Rect(0, 0, parsed.Width, parsed.Height))}
	err = vp8.Decode(truncVP8, nil, nil, probe)
	if err == nil {
		t.Fatal("truncated stream decoded without error")
	}
	if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
		t.Fatalf("truncated decode code %v (%v), want NotEnoughData", code, err)
	}
	if probe.committed == 0 {
		t.Fatal("truncated stream committed no rows; adjust the cut")
	}
	// Stopping at the last good batch returns nil even though the input is
	// truncated, and the prefix matches the intact stream.
	goodImg, good := newMarkedSink(parsed.Width, parsed.Height, probe.committed, 0)
	if err := vp8.Decode(parsed.VP8, nil, nil, good); err != nil {
		t.Fatal(err)
	}
	stopImg, stop := newMarkedSink(parsed.Width, parsed.Height, probe.committed, 0)
	if err := vp8.Decode(truncVP8, nil, nil, stop); err != nil {
		t.Fatalf("stop before truncation: %v, want nil", err)
	}
	if !bytes.Equal(stopImg.Pix[:probe.committed*stopImg.Stride], goodImg.Pix[:probe.committed*goodImg.Stride]) {
		t.Fatal("stopped prefix of truncated stream differs from the intact stream")
	}
	// Running past the good rows still surfaces the corruption, even if the
	// sink only asks for the full frame height.
	far := &rowStopSink{img: image.NewNRGBA(image.Rect(0, 0, parsed.Width, parsed.Height))}
	if err := vp8.Decode(truncVP8, nil, nil, far); err == nil {
		t.Fatal("truncated stream decoded fully without an error")
	} else if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
		t.Fatalf("full truncated decode code %v, want NotEnoughData", code)
	}
	// The sentinel itself is never confused with a status error.
	if !errors.Is(pipeline.ErrStop, pipeline.ErrStop) {
		t.Fatal("ErrStop identity broken")
	}
}

// TestTruncatedFileStillErrorsAtPublicAPI checks the whole-Decoder path on a
// real truncated RIFF fixture: the corruption is reported (not swallowed as
// a stop) and the status code stays NotEnoughData.
func TestTruncatedFileStillErrorsAtPublicAPI(t *testing.T) {
	data, err := os.ReadFile("testdata/alpha_truncated_both.webp")
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if _, err := dec.Decode(); err == nil {
		t.Fatal("truncated fixture decoded without an error")
	} else if code := status.CodeOf(err); code != utils.Vp8StatusNotEnoughData {
		t.Fatalf("code %v (%v), want NotEnoughData", code, err)
	}
}

// TestStopReleasesGoroutinesUnderBudget forces the threaded path with a
// two-helper budget and an emitter, stopping repeatedly, and asserts no
// goroutine or budget leak after each iteration.
func TestStopReleasesGoroutinesUnderBudget(t *testing.T) {
	data := readFile(t, "m4_q75.webp")
	parsed, err := container.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	old := workers.SetLimitForTest(2)
	t.Cleanup(func() { workers.SetLimitForTest(old) })
	base := runtime.NumGoroutine()
	for round := 0; round < 12; round++ {
		_, s := newMarkedSink(parsed.Width, parsed.Height, 1+round*37, 0)
		done := make(chan error, 1)
		go func() { done <- vp8.Decode(parsed.VP8, nil, &vp8.Options{UseThreads: true}, s) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: decode hung", round)
		}
		if n := workers.InUse(); n != 0 {
			t.Fatalf("round %d: %d workers in use", round, n)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("%d goroutines leaked", n-base)
	}
}
