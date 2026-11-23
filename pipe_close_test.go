// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package sound

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper: run fn in a goroutine and fail if it does not return within d.
func mustReturn(t *testing.T, d time.Duration, fn func(), what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// A blocked Receive with no data pending must return (0, io.EOF) once
// the write side closes the pipe.
func TestPipeCloseUnblocksReader(t *testing.T) {
	for _, form := range []Form{MonoCd(), StereoCd()} {
		r, w := Pipe(form)
		var n int
		var err error
		done := make(chan struct{})
		go func() {
			n, err = r.Receive(make([]float64, 4*form.Channels()))
			close(done)
		}()
		time.Sleep(20 * time.Millisecond)
		if cerr := w.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%v: blocked Receive was not released by Close", form)
		}
		if n != 0 || err != io.EOF {
			t.Fatalf("%v: got (n=%d, err=%v), want (0, io.EOF)", form, n, err)
		}
	}
}

// A blocked Send must return io.EOF once the pipe is closed.  Send only
// blocks when internal buffering pressure exists, which is exercised via
// the concurrent stress test below; here verify the post-close path and
// the release guarantee directly through a reader holding the lock.
func TestPipeSendAfterCloseReturnsEOF(t *testing.T) {
	r, w := Pipe(MonoCd())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Send([]float64{1, 2, 3}); err != io.EOF {
		t.Fatalf("Send after Close: got %v want io.EOF", err)
	}
	n, err := r.Receive(make([]float64, 3))
	if n != 0 || err != io.EOF {
		t.Fatalf("Receive after Close with no data: got (%d,%v) want (0,EOF)", n, err)
	}
}

// A Send which is still in flight when the pipe is closed must not hang:
// either its frames get queued and delivered, or it returns io.EOF, in
// finite time.
func TestPipeCloseReleasesInFlightSend(t *testing.T) {
	r, w := Pipe(MonoCd())
	var serr error
	mustReturn(t, 3*time.Second, func() {
		// Fire many Sends from one goroutine while nobody reads, then
		// close from another.  The writer must always return.
		go func() {
			time.Sleep(20 * time.Millisecond)
			w.Close()
		}()
		buf := make([]float64, 8)
		for i := range buf {
			buf[i] = float64(i)
		}
		for {
			if e := w.Send(buf); e != nil {
				serr = e
				return
			}
		}
	}, "in-flight Send after Close")
	if serr != io.EOF {
		t.Fatalf("want io.EOF after close, got %v", serr)
	}
	// Residual queued data must still drain to EOF without hanging.
	mustReturn(t, 3*time.Second, func() {
		buf := make([]float64, 4)
		for {
			if _, e := r.Receive(buf); e == io.EOF {
				return
			}
		}
	}, "drain after Close")
}

// Closing twice must be safe and idempotent.
func TestPipeDoubleClose(t *testing.T) {
	_, w := Pipe(MonoCd())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
}

// Data written before Close that has not yet been read must be delivered
// first, in order and without duplication or loss; EOF comes only after
// the residual frames are drained.
func TestPipeResidualDataBeforeEOF(t *testing.T) {
	for _, tc := range []struct {
		name     string
		form     Form
		sendFrms int
		readFrms int
	}{
		{"mono exact", MonoCd(), 8, 8},
		{"mono larger read", MonoCd(), 5, 16},
		{"mono smaller reads", MonoCd(), 10, 3},
		{"stereo exact", StereoCd(), 6, 6},
		{"stereo larger read", StereoCd(), 4, 9},
		{"stereo smaller reads", StereoCd(), 7, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nC := tc.form.Channels()
			r, w := Pipe(tc.form)
			sent := make([]float64, tc.sendFrms*nC)
			for f := 0; f < tc.sendFrms; f++ {
				for c := 0; c < nC; c++ {
					sent[c*tc.sendFrms+f] = float64(f*7 + c)
				}
			}
			if err := w.Send(sent); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			got := make([]float64, 0, tc.sendFrms*nC)
			readFrames := 0
			for {
				buf := make([]float64, tc.readFrms*nC)
				n, err := r.Receive(buf)
				if n > tc.readFrms {
					t.Fatalf("returned %d frames > requested %d", n, tc.readFrms)
				}
				if n == 0 {
					if err != io.EOF {
						t.Fatalf("want EOF, got %v", err)
					}
					break
				}
				if err != nil {
					t.Fatalf("unexpected err %v with n=%d", err, n)
				}
				// block is de-interleaved over n frames; re-interleave canonically
				for f := 0; f < n; f++ {
					for c := 0; c < nC; c++ {
						got = append(got, buf[c*n+f])
					}
				}
				readFrames += n
			}
			if readFrames != tc.sendFrms {
				t.Fatalf("got %d frames want %d", readFrames, tc.sendFrms)
			}
			for f := 0; f < tc.sendFrms; f++ {
				for c := 0; c < nC; c++ {
					gv := got[f*nC+c]
					wv := float64(f*7 + c)
					if gv != wv {
						t.Errorf("frame %d ch %d: got %.0f want %.0f", f, c, gv, wv)
					}
				}
			}
		})
	}
}

// Many writers and many readers with different buffer sizes, closed while
// traffic is flowing: every goroutine must return in finite time and every
// delivered frame must be read exactly once (no loss, no duplication).
func TestPipeConcurrentClose(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		form := MonoCd()
		if trial%2 == 1 {
			form = StereoCd()
		}
		nC := form.Channels()
		r, w := Pipe(form)
		var sentFrames, recvFrames int64
		stop := make(chan struct{})
		var wg sync.WaitGroup
		const nw, nr = 6, 6
		for i := 0; i < nw; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				frms := 1 + seed%4
				buf := make([]float64, frms*nC)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := w.Send(buf); err != nil {
						return
					}
					atomic.AddInt64(&sentFrames, int64(frms))
				}
			}(i)
		}
		for i := 0; i < nr; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				frms := 1 + seed%5
				buf := make([]float64, frms*nC)
				for {
					n, err := r.Receive(buf)
					if err != nil {
						if err != io.EOF {
							t.Errorf("unexpected recv err %v", err)
						}
						return
					}
					atomic.AddInt64(&recvFrames, int64(n))
				}
			}(i)
		}
		time.Sleep(20 * time.Millisecond)
		close(stop)
		w.Close()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: goroutines deadlocked after Close (sent=%d recv=%d)",
				trial, atomic.LoadInt64(&sentFrames), atomic.LoadInt64(&recvFrames))
		}
		// Everything handed to Send before Close must have been delivered:
		// receivers stop on EOF, so received frames equal accepted frames.
		if got, want := atomic.LoadInt64(&recvFrames), atomic.LoadInt64(&sentFrames); got != want {
			t.Fatalf("trial %d: recv %d frames != sent %d frames", trial, got, want)
		}
	}
}

// Misaligned slices keep the original error semantics.
func TestPipeAlignmentErrors(t *testing.T) {
	r, w := Pipe(StereoCd())
	if err := w.Send([]float64{1, 2, 3}); err != ErrChannelAlignment {
		t.Fatalf("Send alignment: got %v", err)
	}
	if _, err := r.Receive([]float64{1, 2, 3}); err != ErrChannelAlignment {
		t.Fatalf("Receive alignment: got %v", err)
	}
}

// An empty Send on an open pipe is a no-op; on a closed pipe it is EOF.
func TestPipeEmptySend(t *testing.T) {
	r, w := Pipe(MonoCd())
	if err := w.Send(nil); err != nil {
		t.Fatalf("empty Send on open pipe: %v", err)
	}
	w.Close()
	if err := w.Send(nil); err != io.EOF {
		t.Fatalf("empty Send on closed pipe: got %v want EOF", err)
	}
	if _, err := r.Receive(make([]float64, 1)); err != io.EOF {
		t.Fatalf("Receive: got %v want EOF", err)
	}
}
