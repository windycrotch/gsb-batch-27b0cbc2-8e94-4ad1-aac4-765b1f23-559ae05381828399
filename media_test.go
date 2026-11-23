package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

// fakeUDPConn is a scripted udpConn. Each ReadFromUDP call returns the next
// entry of reads; once exhausted it either blocks or returns tailErr.
type fakeUDPConn struct {
	mu      sync.Mutex
	reads   []fakeRead
	idx     int
	tailErr error
	block   bool
	closed  bool
	closeCh chan struct{}
}

type fakeRead struct {
	n   int
	err error
}

func newFakeConn() *fakeUDPConn {
	return &fakeUDPConn{closeCh: make(chan struct{})}
}

func (c *fakeUDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	c.mu.Lock()
	if c.idx < len(c.reads) {
		read := c.reads[c.idx]
		c.idx++
		c.mu.Unlock()
		if read.err != nil {
			return 0, nil, read.err
		}
		// Copy a deterministic payload into the caller buffer.
		if read.n > 0 {
			for i := 0; i < read.n && i < len(b); i++ {
				b[i] = byte(i + 1)
			}
		}
		return read.n, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}, nil
	}
	block := c.block
	c.mu.Unlock()

	if block {
		<-c.closeCh
		return 0, nil, io.ErrClosedPipe
	}
	return 0, nil, c.tailErr
}

func (c *fakeUDPConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.closeCh)
	}
	return nil
}

func (c *fakeUDPConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// dialAttempt describes what one dialUDP call should return.
type dialAttempt struct {
	conn udpConn
	err  error
}

// scriptedDialer returns the configured attempts in order; calls beyond the
// script return tailErr (or block on tailConn when set). The returned function
// reports the number of dial calls and is safe to call concurrently.
func scriptedDialer(attempts []dialAttempt, tailErr error, tailConn udpConn) (dialUDPFn, func() int) {
	calls := 0
	var mu sync.Mutex
	dialer := func(ip string, port int) (udpConn, error) {
		mu.Lock()
		idx := calls
		calls++
		mu.Unlock()
		if idx < len(attempts) {
			return attempts[idx].conn, attempts[idx].err
		}
		if tailConn != nil {
			return tailConn, nil
		}
		return nil, tailErr
	}
	return dialer, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func newTestMediaSource(t *testing.T, kind string, dialer dialUDPFn, maxFailures int) *MediaSource {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "video/VP8"}, kind, "test-"+kind)
	if err != nil {
		t.Fatalf("create track: %v", err)
	}
	m := NewMediaSource(kind, "127.0.0.1", 19999, track)
	m.maxFailures = maxFailures
	m.retryInterval = time.Millisecond
	m.recoveryInterval = time.Millisecond
	if dialer != nil {
		m.dialUDP = dialer
	}
	noSleep := func(time.Duration) bool { return true }
	m.sleep = noSleep
	return m
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// TestMediaSourceDeliversPackets: a healthy socket delivers packets to the
// track and the source reports the active state.
func TestMediaSourceDeliversPackets(t *testing.T) {
	conn := newFakeConn()
	conn.block = true
	conn.reads = []fakeRead{{n: 100}, {n: 100}, {n: 100}}
	dialer, calls := scriptedDialer([]dialAttempt{{conn: conn}}, nil, nil)

	m := newTestMediaSource(t, "video", dialer, 3)
	go m.Run()

	waitFor(t, time.Second, func() bool { return m.Snapshot().PacketsReceived == 3 }, "3 packets received")
	if state := m.Snapshot().State; state != MediaStateActive {
		t.Fatalf("state = %s, want active", state)
	}
	if calls() != 1 {
		t.Fatalf("dial calls = %d, want 1", calls())
	}

	m.Stop()
	if !conn.isClosed() {
		t.Fatal("UDP connection was not closed on Stop")
	}
}

// TestMediaSourceReadErrorRetries: a transient read error closes the broken
// socket, rebinds and resumes reception within the failure budget.
func TestMediaSourceReadErrorRetries(t *testing.T) {
	readErr := errors.New("simulated read failure")

	first := newFakeConn()
	first.reads = []fakeRead{{n: 50}, {err: readErr}}

	second := newFakeConn()
	second.block = true
	second.reads = []fakeRead{{n: 60}, {n: 60}}

	dialer, calls := scriptedDialer([]dialAttempt{{conn: first}, {conn: second}}, nil, nil)
	m := newTestMediaSource(t, "video", dialer, 3)
	go m.Run()

	waitFor(t, time.Second, func() bool { return calls() >= 2 }, "rebind after read error")
	waitFor(t, time.Second, func() bool { return m.Snapshot().PacketsReceived == 3 }, "resume reading packets")

	if !first.isClosed() {
		t.Fatal("broken connection was not closed")
	}
	if state := m.Snapshot().State; state != MediaStateActive {
		t.Fatalf("state = %s, want active after recovery", state)
	}
	if failures := m.Snapshot().Failures; failures != 0 {
		t.Fatalf("failures = %d, want 0 after successful reads", failures)
	}
	m.Stop()
}

// TestMediaSourceBoundedBindRetries: repeated bind failures are retried only
// maxFailures times, after which the source parks in the stopped state and
// keeps the process (and HTTP service) alive.
func TestMediaSourceBoundedBindRetries(t *testing.T) {
	bindErr := errors.New("address already in use")
	dialer, calls := scriptedDialer(nil, bindErr, nil)
	m := newTestMediaSource(t, "video", dialer, 3)
	go m.Run()

	waitFor(t, time.Second, func() bool { return m.Snapshot().State == MediaStateStopped }, "stopped state")
	// Exactly maxFailures dials happen during the bounded phase; further dials
	// belong to the background recovery loop, which keeps retrying.
	if got := calls(); got < 3 {
		t.Fatalf("dial calls = %d, want at least 3 bounded retries", got)
	}
	m.Stop()
	if got := m.Snapshot().State; got != MediaStateStopped {
		t.Fatalf("state after budget = %s, want stopped", got)
	}
}

// TestMediaSourceRecoveryAfterExhaustion: once parked, a later successful bind
// brings the media path back to active and packets flow again.
func TestMediaSourceRecoveryAfterExhaustion(t *testing.T) {
	bindErr := errors.New("network unreachable")

	recovered := newFakeConn()
	recovered.block = true
	recovered.reads = []fakeRead{{n: 42}, {n: 42}}

	// The 3 bounded dials fail and park the source; the next dial from the
	// background recovery loop hands back a working connection.
	dialer, calls := scriptedDialer(
		[]dialAttempt{{err: bindErr}, {err: bindErr}, {err: bindErr}, {conn: recovered}},
		bindErr, nil)
	m := newTestMediaSource(t, "video", dialer, 3)
	go m.Run()

	// The three bounded dials exhaust the budget and park the source.
	waitFor(t, time.Second, func() bool { return calls() >= 3 }, "bounded budget exhausted")
	// The background recovery loop then performs further dials and recovers.
	waitFor(t, 2*time.Second, func() bool { return m.Snapshot().State == MediaStateActive }, "recovered active state")
	waitFor(t, time.Second, func() bool { return m.Snapshot().PacketsReceived == 2 }, "packets after recovery")
	if got := calls(); got < 4 {
		t.Fatalf("dial calls = %d, expected a recovery attempt after the budget", got)
	}
	m.Stop()
}

// TestMediaSourceStopDuringRetry: stopping while a retry sleep is in progress
// unblocks and terminates the loop without leaving the goroutine behind.
func TestMediaSourceStopDuringRetry(t *testing.T) {
	bindErr := errors.New("address already in use")
	dialer, _ := scriptedDialer(nil, bindErr, nil)
	m := newTestMediaSource(t, "video", dialer, 100)

	// A sleep that blocks until Stop.
	m.sleep = func(time.Duration) bool {
		select {
		case <-m.stopCh:
			return false
		case <-time.After(time.Second):
			return true
		}
	}

	done := make(chan struct{})
	go func() { m.Run(); close(done) }()

	waitFor(t, time.Second, func() bool { return m.Snapshot().State == MediaStateReconnecting }, "reconnecting state")
	m.Stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after Stop")
	}
}

// TestMediaStateMarshalsByName ensures the observable status is readable
// rather than an opaque enum number.
func TestMediaStateMarshalsByName(t *testing.T) {
	cases := map[MediaState]string{
		MediaStateActive:       `"active"`,
		MediaStateReconnecting: `"reconnecting"`,
		MediaStateStopped:      `"stopped"`,
	}
	for state, want := range cases {
		got, err := state.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal %d: %v", state, err)
		}
		if string(got) != want {
			t.Fatalf("state %d marshalled as %s, want %s", state, got, want)
		}
		var decoded MediaState
		if err := decoded.UnmarshalJSON(got); err != nil {
			t.Fatalf("unmarshal %s: %v", got, err)
		}
		if decoded != state {
			t.Fatalf("round-trip gave %d, want %d", decoded, state)
		}
	}

	// Numeric (legacy) payloads stay decodable, unknown values error out.
	var legacy MediaState
	if err := legacy.UnmarshalJSON([]byte("2")); err != nil || legacy != MediaStateStopped {
		t.Fatalf("legacy numeric decode = %d, err %v", legacy, err)
	}
	var bad MediaState
	if err := bad.UnmarshalJSON([]byte(`"nope"`)); err == nil {
		t.Fatal("expected error for unknown media state")
	}
}

// packets sent to the bound socket are counted.
func TestMediaSourceRealUDPIntegration(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "video/VP8"}, "video", "integ")
	if err != nil {
		t.Fatalf("create track: %v", err)
	}
	m := NewMediaSource("video", "127.0.0.1", 0, track)
	m.maxFailures = 3
	m.retryInterval = time.Millisecond
	m.recoveryInterval = time.Millisecond
	go m.Run()

	var addr *net.UDPAddr
	waitFor(t, 2*time.Second, func() bool {
		snap := m.Snapshot()
		if snap.State != MediaStateActive || snap.Address == "" {
			return false
		}
		parsed, perr := net.ResolveUDPAddr("udp", snap.Address)
		if perr != nil {
			return false
		}
		addr = parsed
		return true
	}, "real UDP socket active")

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()
	payload := make([]byte, 12)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := 0; i < 5; i++ {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write udp: %v", err)
		}
	}

	waitFor(t, 2*time.Second, func() bool { return m.Snapshot().PacketsReceived == 5 }, "real packets received")
	m.Stop()
}
