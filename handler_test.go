// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

// fakeEndpoint is a controllable Endpoint that records every interaction.
// Its own mutex makes write-after-close and double-close detectable, which
// lets the concurrency test assert ownership guarantees instead of just
// running under -race.
type fakeEndpoint struct {
	name string

	mu        sync.Mutex
	closed    bool
	closeCnt  int
	audioCnt  int
	videoCnt  int
	frameCnt  int
	failWrite bool // when true, every write returns errWriteFailed

	// optional hook invoked while the write lock is held, to exercise
	// interleavings deterministically.
	onWrite func()
}

var errWriteFailed = errors.New("simulated endpoint write failure")

func (f *fakeEndpoint) WriteAudio(uint32, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("write into closed endpoint: " + f.name)
	}
	if f.onWrite != nil {
		f.onWrite()
	}
	if f.failWrite {
		return errWriteFailed
	}
	f.audioCnt++
	return nil
}

func (f *fakeEndpoint) WriteVideo(uint32, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("write into closed endpoint: " + f.name)
	}
	if f.onWrite != nil {
		f.onWrite()
	}
	if f.failWrite {
		return errWriteFailed
	}
	f.videoCnt++
	return nil
}

func (f *fakeEndpoint) WriteSetFrame(uint32, *rtmpmsg.DataMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("write into closed endpoint: " + f.name)
	}
	if f.onWrite != nil {
		f.onWrite()
	}
	if f.failWrite {
		return errWriteFailed
	}
	f.frameCnt++
	return nil
}

func (f *fakeEndpoint) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closeCnt++
}

func (f *fakeEndpoint) counts() (audio, video, frame, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audioCnt, f.videoCnt, f.frameCnt, f.closeCnt
}

func newTestHandler() *Handler {
	return &Handler{HandleFunc: func(*Handler) error { return nil }}
}

// A single failing endpoint must be isolated: it gets closed and excluded
// from later writes, the healthy endpoints keep receiving the whole stream,
// and the inbound handler call never returns an error.
func TestWriteFailureIsolatesOnlyFailingEndpoint(t *testing.T) {
	h := newTestHandler()

	good1 := &fakeEndpoint{name: "good1"}
	bad := &fakeEndpoint{name: "bad", failWrite: true}
	good2 := &fakeEndpoint{name: "good2"}
	for _, ep := range []Endpoint{good1, bad, good2} {
		if err := h.AddEndpoint(ep); err != nil {
			t.Fatalf("AddEndpoint: %v", err)
		}
	}

	// First audio message: bad fails and must be dropped, no error escapes.
	if err := h.OnAudio(0, bytes.NewReader(nil)); err != nil {
		t.Fatalf("OnAudio must not fail when one endpoint fails, got %v", err)
	}

	if _, _, _, closes := bad.counts(); closes != 1 {
		t.Fatalf("failing endpoint closed once, got %d closes", closes)
	}
	if a, _, _, _ := good1.counts(); a != 1 {
		t.Fatalf("healthy endpoint 1 expected 1 audio write, got %d", a)
	}
	if a, _, _, _ := good2.counts(); a != 1 {
		t.Fatalf("healthy endpoint 2 expected 1 audio write, got %d", a)
	}

	// Further writes only reach the two healthy endpoints.
	if err := h.OnVideo(1, bytes.NewReader(nil)); err != nil {
		t.Fatalf("OnVideo: %v", err)
	}
	if err := h.OnSetDataFrame(2, &rtmpmsg.NetStreamSetDataFrame{}); err != nil {
		t.Fatalf("OnSetDataFrame: %v", err)
	}

	if _, v, _, _ := good1.counts(); v != 1 {
		t.Fatalf("healthy endpoint 1 expected 1 video write, got %d", v)
	}
	if _, _, fr, _ := good2.counts(); fr != 1 {
		t.Fatalf("healthy endpoint 2 expected 1 set-frame write, got %d", fr)
	}
	if a, v, fr, _ := bad.counts(); a != 0 || v != 0 || fr != 0 {
		t.Fatalf("excluded endpoint must receive no more writes, got a=%d v=%d f=%d", a, v, fr)
	}

	// Final shutdown must not double-close the already-dropped endpoint.
	h.Close()
	if _, _, _, closes := bad.counts(); closes != 1 {
		t.Fatalf("dropped endpoint closed more than once: %d", closes)
	}
	for _, ep := range []*fakeEndpoint{good1, good2} {
		if _, _, _, closes := ep.counts(); closes != 1 {
			t.Fatalf("healthy endpoint %s closed once, got %d", ep.name, closes)
		}
	}
}

// Repeated / concurrent Close calls close every endpoint exactly once and
// run the afterClose hooks at most once.
func TestCloseIsIdempotentAndHooksRunOnce(t *testing.T) {
	h := newTestHandler()
	eps := []*fakeEndpoint{{name: "a"}, {name: "b"}, {name: "c"}}
	for _, ep := range eps {
		_ = h.AddEndpoint(ep)
	}

	var hookCnt int64
	h.HooksOnClose = append(h.HooksOnClose, func() { atomic.AddInt64(&hookCnt, 1) })
	h.HooksOnClose = append(h.HooksOnClose, func() { atomic.AddInt64(&hookCnt, 1) })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Close()
			h.OnClose() // go-rtmp may also invoke OnClose
		}()
	}
	wg.Wait()

	for _, ep := range eps {
		if _, _, _, closes := ep.counts(); closes != 1 {
			t.Fatalf("endpoint %s closed %d times, want exactly 1", ep.name, closes)
		}
	}
	if got := atomic.LoadInt64(&hookCnt); got != 2 {
		t.Fatalf("afterClose hooks ran %d times, want exactly 2 (each hook once)", got)
	}
}

// After Close, newly established endpoints must be rejected and closed
// immediately rather than leaked.
func TestAddEndpointAfterCloseIsRejected(t *testing.T) {
	h := newTestHandler()
	first := &fakeEndpoint{name: "first"}
	_ = h.AddEndpoint(first)
	h.Close()

	late := &fakeEndpoint{name: "late"}
	if err := h.AddEndpoint(late); err == nil {
		t.Fatal("AddEndpoint on closed handler must return an error")
	}
	if _, _, _, closes := late.counts(); closes != 1 {
		t.Fatalf("rejected endpoint must be closed, got %d closes", closes)
	}

	// Writes after close are no-ops and never touch closed endpoints.
	if err := h.OnAudio(0, bytes.NewReader(nil)); err != nil {
		t.Fatalf("OnAudio after close: %v", err)
	}
	if a, _, _, _ := first.counts(); a != 0 {
		t.Fatalf("closed endpoint must not be written, got %d audio writes", a)
	}
}

// Concurrent writers interleaved with Close: no write may reach a closed
// endpoint, every endpoint is closed exactly once, and no healthy endpoint
// ever sees an error surface through the handler.
func TestConcurrentWritesAndClose(t *testing.T) {
	h := newTestHandler()

	var endpoints []*fakeEndpoint
	for i := 0; i < 8; i++ {
		ep := &fakeEndpoint{name: "ep"}
		endpoints = append(endpoints, ep)
		_ = h.AddEndpoint(ep)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				// Any error here would mean the fanout let an endpoint
				// failure escape; isolation must keep these nil.
				if err := h.OnAudio(1, bytes.NewReader([]byte{1, 2, 3})); err != nil {
					t.Errorf("OnAudio returned error during close race: %v", err)
					return
				}
			}
		}()
	}

	// Let writes run for a bit, then race Close against them.
	time.Sleep(20 * time.Millisecond)
	h.Close()
	time.Sleep(5 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	for _, ep := range endpoints {
		if _, _, _, closes := ep.counts(); closes != 1 {
			t.Fatalf("endpoint %s closed %d times, want exactly 1", ep.name, closes)
		}
	}
}

// A failing endpoint removed mid-stream while other writers are active must
// be closed exactly once and never receive another write.
func TestConcurrentFailureAndWrites(t *testing.T) {
	h := newTestHandler()

	flaky := &fakeEndpoint{name: "flaky"}
	good := &fakeEndpoint{name: "good"}
	_ = h.AddEndpoint(flaky)
	_ = h.AddEndpoint(good)

	// Block flaky's first write until other writers are queued, then fail it.
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})
	flaky.onWrite = func() {
		startOnce.Do(func() { close(started) })
		<-release
	}
	flaky.failWrite = true

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = h.OnVideo(0, bytes.NewReader(nil)) // blocks inside flaky's write, then drops it
	}()

	<-started
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.OnAudio(0, bytes.NewReader(nil))
		}()
	}
	close(release)
	wg.Wait()

	if _, _, _, closes := flaky.counts(); closes != 1 {
		t.Fatalf("flaky endpoint closed %d times, want exactly 1", closes)
	}
	if a, _, _, _ := flaky.counts(); a != 0 {
		t.Fatalf("dropped endpoint received audio writes after failure: %d", a)
	}
	if a, _, _, _ := good.counts(); a == 0 {
		t.Fatal("healthy endpoint must keep receiving the stream")
	}
}
