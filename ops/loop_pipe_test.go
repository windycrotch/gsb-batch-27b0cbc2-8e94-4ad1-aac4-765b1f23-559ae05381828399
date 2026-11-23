// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zikichombo/sound"
	"github.com/zikichombo/sound/freq"
)

// expectedRepeated builds the canonical interleaved expectation: source
// pattern repeated "times" times.
func expectedRepeated(nCh, srcLen, times, base int) []float64 {
	out := make([]float64, 0, srcLen*times*nCh)
	for f := 0; f < srcLen*times; f++ {
		sf := f % srcLen
		for c := 0; c < nCh; c++ {
			out = append(out, float64(base*c+sf+1))
		}
	}
	return out
}

// deinterToInter converts a de-interleaved block of n frames/nC channels
// into canonical interleaved order.
func deinterToInter(block []float64, nC, n int) []float64 {
	out := make([]float64, 0, n*nC)
	for f := 0; f < n; f++ {
		for c := 0; c < nC; c++ {
			out = append(out, block[c*n+f])
		}
	}
	return out
}

// TestLoopIntoPipe drives a Loop's output into the write end of a Pipe and
// consumes the read end through buffers of varying sizes across goroutines.
// After Close the consumer must see every frame exactly once, in order,
// followed by io.EOF, with no deadlock.
func TestLoopIntoPipe(t *testing.T) {
	base := 700
	for _, nCh := range []int{1, 2, 3} {
		for _, srcLen := range []int{1, 3, 5} {
			for _, times := range []int{1, 2, 4} {
				for _, sendFrames := range []int{1, 4} {
					for _, recvFrames := range []int{1, 2, 7} {
						name := logName(nCh, srcLen, times, recvFrames) +
							" send=" + itoa(sendFrames)
						src := patSrc(nCh, srcLen, base)
						lp := Loop(src, times)
						r, w := sound.Pipe(sound.NewForm(freq.Hertz, nCh))

						var wg sync.WaitGroup
						wg.Add(1)
						go func() {
							defer wg.Done()
							buf := make([]float64, sendFrames*nCh)
							for {
								n, err := lp.Receive(buf)
								if err == io.EOF {
									w.Close()
									return
								}
								if err != nil {
									t.Errorf("%s: loop receive: %v", name, err)
									w.Close()
									return
								}
								if serr := w.Send(buf[:n*nCh]); serr != nil {
									t.Errorf("%s: pipe send: %v", name, serr)
									return
								}
							}
						}()

						var got []float64
						consumerDone := make(chan struct{})
						go func() {
							defer close(consumerDone)
							buf := make([]float64, recvFrames*nCh)
							for {
								n, err := r.Receive(buf)
								if n > recvFrames {
									t.Errorf("%s: got %d > requested %d frames",
										name, n, recvFrames)
									return
								}
								if n == 0 {
									if err != io.EOF {
										t.Errorf("%s: want EOF got %v", name, err)
									}
									return
								}
								if err != nil && err != io.EOF {
									t.Errorf("%s: recv err %v", name, err)
									return
								}
								got = append(got, deinterToInter(buf[:n*nCh], nCh, n)...)
							}
						}()

						select {
						case <-consumerDone:
						case <-time.After(10 * time.Second):
							t.Fatalf("%s: consumer deadlocked", name)
						}
						wg.Wait()

						want := expectedRepeated(nCh, srcLen, times, base)
						if len(got) != len(want) {
							t.Fatalf("%s: got %d samples want %d", name, len(got), len(want))
						}
						for i := range want {
							if got[i] != want[i] {
								t.Errorf("%s: sample %d got %.0f want %.0f",
									name, i, got[i], want[i])
							}
						}
					}
				}
			}
		}
	}
}

// TestLoopIntoPipeAbruptClose closes the pipe while the loop is still
// producing: the producer must unblock with io.EOF and the consumer must
// still drain every frame that was accepted, then get io.EOF, with no
// deadlock or panic.
func TestLoopIntoPipeAbruptClose(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		nCh := 1 + trial%3
		// An effectively infinite source keeps producing until close.
		src := patSrc(nCh, 7, 300)
		lp := Loop(src, -1)
		r, w := sound.Pipe(sound.NewForm(freq.Hertz, nCh))

		var accepted int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]float64, 3*nCh)
			for {
				n, err := lp.Receive(buf)
				if err != nil {
					return
				}
				if serr := w.Send(buf[:n*nCh]); serr != nil {
					return // io.EOF once closed
				}
				accepted += int64(n)
			}
		}()

		var consumed int64
		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			buf := make([]float64, 5*nCh)
			for {
				n, err := r.Receive(buf)
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Errorf("recv: %v", err)
					return
				}
				consumed += int64(n)
			}
		}()

		time.Sleep(10 * time.Millisecond)
		w.Close()
		select {
		case <-consumerDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: consumer did not reach EOF after Close", trial)
		}
		wg.Wait()
		if consumed != accepted {
			t.Fatalf("trial %d: consumed %d frames but producer delivered %d",
				trial, consumed, accepted)
		}
	}
}
