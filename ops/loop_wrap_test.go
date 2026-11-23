// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"io"
	"testing"

	"github.com/zikichombo/sound"
	"github.com/zikichombo/sound/freq"
	"github.com/zikichombo/sound/sndbuf"
)

// patSrc builds an nCh-channel source of srcLen frames whose sample for
// channel c at frame f equals base*c + f+1 (all positive and distinct per
// channel, so any channel crossing is detected).  Backing data is
// interleaved as required by sndbuf.FromSliceChans.
func patSrc(nCh, srcLen, base int) *sndbuf.T {
	d := make([]float64, srcLen*nCh)
	for f := 0; f < srcLen; f++ {
		for c := 0; c < nCh; c++ {
			d[f*nCh+c] = float64(base*c + f + 1)
		}
	}
	return sndbuf.FromSliceChans(d, nCh, freq.Hertz)
}

// drainLoop consumes lp through fixed frame-size buffers and returns the
// full output as a canonical interleaved stream, enforcing:
//   - never more frames than requested,
//   - EOF only alongside 0 frames.
func drainLoop(t *testing.T, lp sound.Source, nCh, bufFrames int) []float64 {
	t.Helper()
	var il []float64
	for {
		buf := make([]float64, bufFrames*nCh)
		n, err := lp.Receive(buf)
		if n > bufFrames {
			t.Fatalf("returned %d frames > requested %d", n, bufFrames)
		}
		if n == 0 {
			if err != io.EOF {
				t.Fatalf("0 frames with err %v, want io.EOF", err)
			}
			return il
		}
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected err %v", err)
		}
		// returned block is de-interleaved over n frames -> canonicalize
		for f := 0; f < n; f++ {
			for c := 0; c < nCh; c++ {
				il = append(il, buf[c*n+f])
			}
		}
		if err == io.EOF {
			t.Fatal("io.EOF returned together with data")
		}
	}
}

func verifyPattern(t *testing.T, name string, il []float64, nCh, srcLen, times, base int) {
	t.Helper()
	wantFrames := times * srcLen
	if len(il)/nCh != wantFrames {
		t.Fatalf("%s: got %d frames want %d", name, len(il)/nCh, wantFrames)
	}
	for f := 0; f < wantFrames; f++ {
		sf := f % srcLen
		for c := 0; c < nCh; c++ {
			got, want := il[f*nCh+c], float64(base*c+sf+1)
			if got != want {
				t.Errorf("%s: frame %d ch %d got %.0f want %.0f", name, f, c, got, want)
			}
		}
	}
}

// Exhaustive matrix: channel count x source length x repeat count x
// receive-buffer size, including buffers smaller and larger than the
// source and enough wraps within one Receive to exercise repeated
// wrap-around.
func TestLoopRepetitionMatrix(t *testing.T) {
	base := 1000
	for _, nCh := range []int{1, 2, 3, 4} {
		for _, srcLen := range []int{1, 2, 3, 5, 8} {
			for _, times := range []int{1, 2, 3, 7} {
				for _, bufFrames := range []int{1, 2, 3, 5, 8, 13, 21} {
					name := logName(nCh, srcLen, times, bufFrames)
					src := patSrc(nCh, srcLen, base)
					lp := Loop(src, times)
					il := drainLoop(t, lp, nCh, bufFrames)
					verifyPattern(t, name, il, nCh, srcLen, times, base)
				}
			}
		}
	}
}

func logName(nCh, srcLen, times, bufFrames int) string {
	return "ch=" + itoa(nCh) + " src=" + itoa(srcLen) +
		" times=" + itoa(times) + " buf=" + itoa(bufFrames)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// A single Receive whose buffer spans several full copies of the source
// must wrap multiple times correctly, including the multi-channel case.
func TestLoopMultipleWrapsInOneReceive(t *testing.T) {
	for _, nCh := range []int{1, 2, 3} {
		srcLen := 3
		times := 6
		// Request strictly more frames than the total output in one call;
		// the loop must wrap multiple times internally and return exactly
		// the available total.
		bufFrames := srcLen*times + 1
		src := patSrc(nCh, srcLen, 500)
		lp := Loop(src, times)
		buf := make([]float64, bufFrames*nCh)
		n, err := lp.Receive(buf)
		if err != nil {
			t.Fatalf("ch=%d: %v", nCh, err)
		}
		if n != srcLen*times {
			t.Fatalf("ch=%d: got %d frames want %d", nCh, n, srcLen*times)
		}
		// buf[:n*nCh] is one de-interleaved block over n frames.
		var il []float64
		for f := 0; f < n; f++ {
			for c := 0; c < nCh; c++ {
				il = append(il, buf[c*n+f])
			}
		}
		verifyPattern(t, "multiwrap", il, nCh, srcLen, times, 500)
		// everything consumed: next call is EOF
		n2, err2 := lp.Receive(buf)
		if n2 != 0 || err2 != io.EOF {
			t.Fatalf("ch=%d: after exhaustion got (%d,%v) want (0,EOF)", nCh, n2, err2)
		}
	}
}

// n == 0 terminates immediately with EOF.
func TestLoopZeroTimes(t *testing.T) {
	src := patSrc(2, 4, 10)
	lp := Loop(src, 0)
	n, err := lp.Receive(make([]float64, 8))
	if n != 0 || err != io.EOF {
		t.Fatalf("got (%d,%v) want (0,EOF)", n, err)
	}
}

// n < 0 loops forever: consume many frames across wrap boundaries and
// confirm the pattern never terminates or corrupts.
func TestLoopInfinite(t *testing.T) {
	nCh, srcLen := 2, 3
	src := patSrc(nCh, srcLen, 100)
	lp := Loop(src, -1)
	il := make([]float64, 0, nCh*srcLen*10)
	buf := make([]float64, 2*nCh) // deliberately crosses wraps mid-buffer
	totalFrames := srcLen * 10
	for len(il)/nCh < totalFrames {
		n, err := lp.Receive(buf)
		if err != nil {
			t.Fatal(err)
		}
		for f := 0; f < n; f++ {
			for c := 0; c < nCh; c++ {
				il = append(il, buf[c*n+f])
			}
		}
	}
	for f := 0; f < totalFrames; f++ {
		sf := f % srcLen
		for c := 0; c < nCh; c++ {
			if il[f*nCh+c] != float64(100*c+sf+1) {
				t.Fatalf("infinite loop frame %d ch %d mismatch", f, c)
			}
		}
	}
}

// A final partial receive after the repeats run out must return the
// remaining frames with nil error, then EOF on the next call.
func TestLoopShortFinalReceive(t *testing.T) {
	// 2 copies of a 4-frame mono source = 8 frames; read 6 then 6.
	src := sndbuf.FromSlice([]float64{0, 1, 2, 3}, freq.Hertz)
	lp := Loop(src, 2)
	first := make([]float64, 6)
	n1, err := lp.Receive(first)
	if err != nil || n1 != 6 {
		t.Fatalf("first receive got (%d,%v)", n1, err)
	}
	for i, v := range first {
		if v != float64(i%4) {
			t.Fatalf("first[%d]=%.0f", i, v)
		}
	}
	second := make([]float64, 6)
	n2, err := lp.Receive(second)
	if err != nil || n2 != 2 {
		t.Fatalf("short receive got (%d,%v) want (2,nil)", n2, err)
	}
	// frames 6 and 7 are source indices 2 and 3.
	if second[0] != 2 || second[1] != 3 {
		t.Fatalf("residual frames wrong: %v", second[:2])
	}
	n3, err := lp.Receive(second)
	if n3 != 0 || err != io.EOF {
		t.Fatalf("final receive got (%d,%v) want (0,EOF)", n3, err)
	}
}
