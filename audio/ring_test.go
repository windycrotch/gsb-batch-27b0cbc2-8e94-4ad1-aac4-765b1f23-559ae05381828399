package audio

import (
	"sync"
	"testing"
)

// TestRingBufferWraparound fills and drains a ring repeatedly to exercise the
// circular head/tail arithmetic.
func TestRingBufferWraparound(t *testing.T) {
	ring := NewRingBuffer(16)
	in := make([]int16, 4)
	for i := range in {
		in[i] = int16(i + 1)
	}
	for cycle := 0; cycle < 10; cycle++ {
		ring.Append(in)
		out := make([]int16, 4)
		n := ring.Extract(out)
		if n != 4 {
			t.Fatalf("cycle %d: extracted %d, want 4", cycle, n)
		}
		for i, v := range out {
			if v != in[i] {
				t.Fatalf("cycle %d sample %d = %d, want %d", cycle, i, v, in[i])
			}
		}
	}
}

// TestRingBufferPartialExtract leaves excess data in place.
func TestRingBufferPartialExtract(t *testing.T) {
	ring := NewRingBuffer(32)
	ring.Append([]int16{1, 2, 3, 4, 5, 6})
	out := make([]int16, 2)
	if n := ring.Extract(out); n != 2 || out[0] != 1 || out[1] != 2 {
		t.Fatalf("unexpected partial extract: n=%d out=%v", n, out)
	}
	out2 := make([]int16, 4)
	if n := ring.Extract(out2); n != 4 {
		t.Fatalf("second extract n=%d", n)
	}
}

// TestRingBufferConcurrent runs a producer and consumer concurrently to catch
// head/tail races under -race.
func TestRingBufferConcurrent(t *testing.T) {
	const total = 2000
	// large enough that the producer never blocks on capacity; the test is
	// about head/tail correctness under -race, not back-pressure.
	ring := NewRingBuffer(8 * total * 8)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]int16, 8)
		for i := 0; i < total; i++ {
			for j := range buf {
				buf[j] = int16(i)
			}
			ring.Append(buf)
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]int16, 8)
		got := 0
		for got < total*8 {
			n := ring.Extract(buf)
			got += n
		}
	}()
	wg.Wait()
}
