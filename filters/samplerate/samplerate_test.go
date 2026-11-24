package samplerate

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/1lann/dissonance/audio"
)

// staticStream serves a fixed slice using small, varying chunk sizes and
// eventually returns io.EOF, mimicking a finite offline source.
type staticStream struct {
	data    []int32
	pos     int
	chunkAt int
}

func (s *staticStream) Read(dst interface{}) (int, error) {
	dstLen := audio.SliceLength(dst)
	chunkSizes := []int{1, 2, 3, 5}
	chunk := chunkSizes[s.chunkAt%len(chunkSizes)]
	s.chunkAt++
	if chunk > dstLen {
		chunk = dstLen
	}
	if chunk > len(s.data)-s.pos {
		chunk = len(s.data) - s.pos
	}
	buf := dst.([]int32)
	copy(buf[:chunk], s.data[s.pos:s.pos+chunk])
	s.pos += chunk
	if s.pos >= len(s.data) {
		return chunk, io.EOF
	}
	return chunk, nil
}

func (s *staticStream) SampleRate() int { return 48000 }

// errAfterStream serves its data (possibly across several reads) and then
// returns a sentinel error.
type errAfterStream struct {
	data []int32
	pos  int
	err  error
}

func (s *errAfterStream) Read(dst interface{}) (int, error) {
	buf := dst.([]int32)
	if s.pos >= len(s.data) {
		return 0, s.err
	}
	n := copy(buf, s.data[s.pos:])
	s.pos += n
	if s.pos >= len(s.data) {
		return n, s.err
	}
	return n, nil
}

func (s *errAfterStream) SampleRate() int { return 48000 }

func makePattern(n int) []int32 {
	out := make([]int32, n)
	for i := range out {
		// Mixed-amplitude deterministic pattern so interpolation matters.
		out[i] = int32((i * 37 % 2001) - 1000)
	}
	return out
}

// drainFilter reads a filter in chunks of the given fixed size until EOF,
// returning every produced output sample in order. A bounded number of
// (0, nil) reads is tolerated because an io.Reader source may legitimately
// hand out its data in tiny pieces; a genuinely stalled filter would
// exhaust the retry budget and fail the test.
func drainFilter(t *testing.T, f audio.Stream, chunk int) []int32 {
	t.Helper()
	var out []int32
	dst := make([]int32, chunk)
	idleReads := 0
	for {
		n, err := f.Read(dst)
		out = append(out, dst[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			idleReads++
			if idleReads > 1000 {
				t.Fatal("filter stalled with nil-error zero reads")
			}
			continue
		}
		idleReads = 0
	}
}

func TestContinuityAcrossReads(t *testing.T) {
	pattern := makePattern(300)
	targetRate := 24000 // ratio 2.0, downsampling

	oneShot := NewFilter(targetRate).Filter(&staticStream{data: append([]int32(nil), pattern...)})
	bigDst := make([]int32, 4096)
	var oneShotOut []int32
	for {
		n, err := oneShot.Read(bigDst)
		oneShotOut = append(oneShotOut, bigDst[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("one-shot read: %v", err)
		}
	}

	chunked := NewFilter(targetRate).Filter(&staticStream{data: append([]int32(nil), pattern...)})
	chunkedOut := make([]int32, 0, len(oneShotOut))
	dstSizes := []int{1, 1, 2, 3, 7, 13}
	dsts := make([][]int32, len(dstSizes))
	for i, sz := range dstSizes {
		dsts[i] = make([]int32, sz)
	}
	i := 0
	for {
		dst := dsts[i%len(dsts)]
		i++
		n, err := chunked.Read(dst)
		chunkedOut = append(chunkedOut, dst[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("chunked read: %v", err)
		}
	}

	if len(chunkedOut) != len(oneShotOut) {
		t.Fatalf("chunked produced %d outputs, one-shot %d (unstable sample count)",
			len(chunkedOut), len(oneShotOut))
	}
	for j := range oneShotOut {
		if chunkedOut[j] != oneShotOut[j] {
			t.Fatalf("output %d diverges across read boundaries: chunked=%d one-shot=%d",
				j, chunkedOut[j], oneShotOut[j])
		}
	}
	if len(oneShotOut) == 0 {
		t.Fatal("expected resampled outputs, got none")
	}
}

func TestOutputGrowsMonotonicallyWithInput(t *testing.T) {
	// Source 48k, target 16k => ratio 3. A finite source that returns
	// (0, io.EOF) only once exhausted keeps the filter from blocking on a
	// stream that has more data coming later.
	pattern := makePattern(90)
	f := NewFilter(16000).Filter(&staticStream{data: pattern})

	dst := make([]int32, 4)
	totalOut := 0
	idleReads := 0
	for {
		n, err := f.Read(dst)
		if n < 0 {
			t.Fatal("negative production")
		}
		totalOut += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			// (0, nil) while the source still has data is legal; the filter
			// is waiting for a neighbour sample. It must not repeat forever.
			idleReads++
			if idleReads > 1000 {
				t.Fatal("filter stalled with nil-error zero reads")
			}
			continue
		}
		idleReads = 0
		// Outputs can never outrun the input and every non-empty Read
		// strictly increases the cumulative output count.
		if totalOut > len(pattern) {
			t.Fatalf("outputs (%d) exceed input samples (%d)", totalOut, len(pattern))
		}
	}
	// 90 input samples at ratio 3 -> interpolation positions 0,3,...,87
	// => exactly 30 outputs, no tail dropped.
	if totalOut != 30 {
		t.Fatalf("total outputs = %d, want 30", totalOut)
	}
}

func TestEOFDoesNotDiscardBufferedTail(t *testing.T) {
	// Downsampling: 4 input samples, ratio 2 -> outputs at positions 0, 2.
	pattern := []int32{1000, 1100, 1200, 1300}
	f := NewFilter(24000).Filter(&staticStream{data: pattern})

	// Request far more than the source can provide.
	dst := make([]int32, 64)
	n, err := f.Read(dst)
	if err != nil {
		t.Fatalf("first Read must return buffered tail before EOF, got err=%v", err)
	}
	if n != 2 {
		t.Fatalf("tail outputs = %d, want 2", n)
	}
	if dst[0] != pattern[0] {
		t.Fatalf("first output = %d, want %d", dst[0], pattern[0])
	}
	wantSecond := pattern[2] // interpolation at integer position 2 is exact
	if dst[1] != wantSecond {
		t.Fatalf("second output = %d, want %d", dst[1], wantSecond)
	}
	n, err = f.Read(dst)
	if n != 0 || err != io.EOF {
		t.Fatalf("after tail: n=%d err=%v, want 0/EOF", n, err)
	}
}

func TestEOFTailOneSampleAtATime(t *testing.T) {
	// Upsampling: 4 input samples, ratio 0.5 -> 6 outputs.
	pattern := []int32{10, 20, 40, 80}
	f := NewFilter(96000).Filter(&staticStream{data: pattern})

	got := drainFilter(t, f, 1)
	if len(got) != 6 {
		t.Fatalf("produced %d outputs, want 6", len(got))
	}
	// Exact values at integer positions must line up across the chunked
	// reads: position 0 -> 10, position 2 -> 40.
	if got[0] != 10 || got[4] != 40 {
		t.Fatalf("interpolation discontinuity: %v", got)
	}
}

func TestUpstreamErrorSurfacesAfterTail(t *testing.T) {
	sentinel := errors.New("samplerate: upstream failure")
	pattern := []int32{1, 2, 3, 4, 5, 6}
	f := NewFilter(24000).Filter(&errAfterStream{data: pattern, err: sentinel})

	var got []int32
	dst := make([]int32, 2)
	for {
		n, err := f.Read(dst)
		got = append(got, dst[:n]...)
		if err != nil {
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want sentinel", err)
			}
			break
		}
		if n == 0 {
			t.Fatal("nil-error zero read")
		}
	}
	if len(got) == 0 {
		t.Fatal("buffered outputs were discarded on upstream error")
	}
	// ratio 2 over 6 inputs -> positions 0, 2, 4 => 3 outputs.
	if len(got) != 3 {
		t.Fatalf("produced %d outputs before error, want 3", len(got))
	}
	// The error is sticky.
	if _, err := f.Read(dst); !errors.Is(err, sentinel) {
		t.Fatalf("terminal error not sticky: %v", err)
	}
}

func TestFilterSampleRate(t *testing.T) {
	src := audio.NewOfflineStream(48000, 16)
	f := NewFilter(16000).Filter(src)
	if f.SampleRate() != 16000 {
		t.Fatalf("SampleRate = %d, want 16000", f.SampleRate())
	}
}

func TestFilterOverOfflineStreamConcurrentClose(t *testing.T) {
	src := audio.NewOfflineStream(48000, 64)
	f := NewFilter(24000).Filter(src)

	writerDone := make(chan struct{})
	go func() {
		for b := 0; b < 20; b++ {
			buf := make([]int32, 12)
			for i := range buf {
				buf[i] = int32(b*12 + i)
			}
			if err := src.WriteValues(buf); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
		src.Close()
		close(writerDone)
	}()

	deadline := time.After(10 * time.Second)
	var total int
	dst := make([]int32, 5)
	for {
		readDone := make(chan struct{})
		var n int
		var err error
		go func() {
			n, err = f.Read(dst)
			close(readDone)
		}()
		select {
		case <-readDone:
		case <-deadline:
			t.Fatal("filter Read blocked past deadline")
		}
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	<-writerDone
	// 240 input samples, ratio 2 -> 120 outputs.
	if total != 120 {
		t.Fatalf("total outputs = %d, want 120", total)
	}
}
