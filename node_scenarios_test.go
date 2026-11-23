package main

import (
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	testTimeout      = 3 * time.Second
	baselineDeadline = 3 * time.Second
)

// waitForHandlerGone blocks until the connection handler is unregistered.
func (h *testHarness) waitForHandlerGone(c *wsClient) {
	h.t.Helper()
	clientPort := c.remotePort()
	waitForCondition(h.t, baselineDeadline, func() bool {
		h.node.mutexConnections.Lock()
		defer h.node.mutexConnections.Unlock()
		for _, handler := range h.node.connections {
			if handler.port == clientPort {
				return false
			}
		}
		return true
	}, "connection handler unregistered")
}

// waitForIPBaseline blocks until the per-IP quota map is empty.
func (h *testHarness) waitForIPBaseline() {
	h.t.Helper()
	waitForCondition(h.t, baselineDeadline, func() bool {
		h.node.mutexIpCount.Lock()
		defer h.node.mutexIpCount.Unlock()
		return len(h.node.ipCount) == 0
	}, "per-IP quota back to baseline")
}

func (h *testHarness) waitForSourceGone(sid string) {
	h.t.Helper()
	waitForCondition(h.t, baselineDeadline, func() bool {
		h.node.mutexStatus.Lock()
		defer h.node.mutexStatus.Unlock()
		return h.node.sources[sid] == nil
	}, "source "+sid+" deregistered")
}

func (h *testHarness) registeredSource(sid string) *WRTC_Source {
	h.node.mutexStatus.Lock()
	defer h.node.mutexStatus.Unlock()
	return h.node.sources[sid]
}

func (h *testHarness) registeredSinks(sid string) int {
	h.node.mutexStatus.Lock()
	defer h.node.mutexStatus.Unlock()
	return len(h.node.sinks[sid])
}

// isSourceReady reports whether the node currently sees the given source as
// ready (its asynchronous ready notification has been processed).
func (h *testHarness) isSourceReady(source *WRTC_Source) bool {
	source.statusMutex.Lock()
	ready := source.ready
	source.statusMutex.Unlock()
	return ready
}

// waitForSourceReady bridges the asynchronous source-ready notification.
func (h *testHarness) waitForSourceReady(source *WRTC_Source) {
	h.t.Helper()
	waitForCondition(h.t, testTimeout, func() bool {
		return h.isSourceReady(source)
	}, "source "+source.sid+" ready")
}

// TestNormalPublishAndPlay drives a full PUBLISH -> ready -> PLAY -> tracks
// flow through the real signaling entry point using fake peers.
func TestNormalPublishAndPlay(t *testing.T) {
	h := newTestHarness(t)

	pub := h.dial()
	pub.publish("pub-1", "stream-a")
	pub.waitFor("OK", "pub-1", testTimeout)
	pub.waitFor("OFFER", "pub-1", testTimeout)

	pubHandler := h.handlerFor(pub)
	if got := requestCountOf(pubHandler); got != 1 {
		t.Fatalf("requestCount = %d, want 1", got)
	}
	if h.ipQuota("127.0.0.1") != 1 {
		t.Fatalf("ip quota = %d, want 1", h.ipQuota("127.0.0.1"))
	}

	source := sourceFor(pubHandler, "pub-1")
	if source == nil {
		t.Fatal("source not bound to request")
	}

	// Complete the handshake: fake source becomes ready with tracks.
	h.factory.fakeSource(source).simulateReady()
	h.waitForSourceReady(source)

	// A player joins while the source is ready: it must get the tracks and an
	// OFFER immediately.
	play := h.dial()
	play.play("play-1", "stream-a")
	play.waitFor("OK", "play-1", testTimeout)
	play.waitFor("STANDBY", "play-1", testTimeout)
	play.waitFor("OFFER", "play-1", testTimeout)

	playHandler := h.handlerFor(play)
	sink := sinkFor(playHandler, "play-1")
	if sink == nil {
		t.Fatal("sink not bound to request")
	}

	waitForCondition(t, testTimeout, func() bool {
		return h.factory.fakeSink(sink) != nil
	}, "sink peer created")

	fp := h.factory.fakeSink(sink)
	videoID, _ := fp.trackIDs()
	if videoID == "" {
		t.Fatal("sink did not receive the source tracks")
	}

	// Explicit CLOSE of the publish request.
	pub.closeRequest("pub-1")
	h.waitForSourceGone("stream-a")
	if requestTypeOf(pubHandler, "pub-1") != 0 {
		t.Fatal("publish request still registered after CLOSE")
	}
	if requestCountOf(pubHandler) != 0 {
		t.Fatalf("requestCount = %d, want 0 after CLOSE", requestCountOf(pubHandler))
	}
	if h.factory.fakeSource(source).isClosed() == false {
		t.Fatal("source peer not closed after CLOSE")
	}

	// The request-id may now be reused.
	pub.publish("pub-1", "stream-b")
	pub.waitFor("OK", "pub-1", testTimeout)
	pub.waitFor("OFFER", "pub-1", testTimeout)
	if requestTypeOf(pubHandler, "pub-1") != REQUEST_TYPE_PUBLISH {
		t.Fatal("request-id not reusable after CLOSE")
	}
	pub.closeRequest("pub-1")
	h.waitForSourceGone("stream-b")

	// Close the player and both sockets; everything must return to baseline.
	play.closeRequest("play-1")
	waitForCondition(t, testTimeout, func() bool {
		return h.registeredSinks("stream-a") == 0
	}, "sink removed after CLOSE")

	pub.shutdown()
	play.shutdown()
	h.waitForHandlerGone(pub)
	h.waitForHandlerGone(play)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestDuplicateRequestIDRejected reuses a request-id without CLOSE. The second
// request must be rejected with PROTOCOL_ERROR and no counter may change.
func TestDuplicateRequestIDRejected(t *testing.T) {
	h := newTestHarness(t)

	c := h.dial()
	defer c.shutdown()

	c.publish("dup", "stream-x")
	c.waitFor("OK", "dup", testTimeout)
	c.waitFor("OFFER", "dup", testTimeout)

	handler := h.handlerFor(c)
	countBefore := requestCountOf(handler)
	sourceBefore := sourceFor(handler, "dup")

	// Reuse the same request-id for a PUBLISH.
	c.publish("dup", "stream-y")
	errMsg := c.waitFor("ERROR", "dup", testTimeout)
	if errMsg.params["error-code"] != "PROTOCOL_ERROR" {
		t.Fatalf("error code = %q, want PROTOCOL_ERROR", errMsg.params["error-code"])
	}
	if requestCountOf(handler) != countBefore {
		t.Fatalf("requestCount changed from %d to %d on rejected request", countBefore, requestCountOf(handler))
	}
	if sourceFor(handler, "dup") != sourceBefore {
		t.Fatal("rejected duplicate replaced the registered source")
	}
	if h.registeredSource("stream-y") != nil {
		t.Fatal("rejected duplicate registered a second source")
	}

	// Same for PLAY on an active PLAY request.
	c.play("play-dup", "stream-x")
	c.waitFor("OK", "play-dup", testTimeout)
	c.waitFor("STANDBY", "play-dup", testTimeout)
	countBefore = requestCountOf(handler)
	c.play("play-dup", "stream-z")
	errMsg = c.waitFor("ERROR", "play-dup", testTimeout)
	if errMsg.params["error-code"] != "PROTOCOL_ERROR" {
		t.Fatalf("error code = %q, want PROTOCOL_ERROR", errMsg.params["error-code"])
	}
	if requestCountOf(handler) != countBefore {
		t.Fatal("requestCount changed on rejected PLAY")
	}

	c.shutdown()
	h.waitForHandlerGone(c)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestHandshakeIncompleteDisconnect opens requests that never complete their
// WebRTC handshake and then drops the socket. The per-IP quota, per-socket
// request count and all registrations must still be fully released.
func TestHandshakeIncompleteDisconnect(t *testing.T) {
	h := newTestHarness(t)

	c := h.dial()

	// One pending publish (OFFER sent, no answer/tracks).
	c.publish("pub-pending", "stream-p")
	c.waitFor("OK", "pub-pending", testTimeout)
	c.waitFor("OFFER", "pub-pending", testTimeout)

	// One pending play (STANDBY, no tracks yet).
	c.play("play-pending", "stream-p")
	c.waitFor("OK", "play-pending", testTimeout)
	c.waitFor("STANDBY", "play-pending", testTimeout)

	handler := h.handlerFor(c)
	if requestCountOf(handler) != 2 {
		t.Fatalf("requestCount = %d, want 2", requestCountOf(handler))
	}
	if h.registeredSinks("stream-p") != 1 {
		t.Fatalf("sink registry = %d, want 1", h.registeredSinks("stream-p"))
	}

	// Abrupt disconnect while neither request completed its handshake.
	c.shutdown()
	h.waitForHandlerGone(c)
	h.waitForIPBaseline()

	waitForCondition(t, baselineDeadline, func() bool {
		return h.registeredSource("stream-p") == nil && h.registeredSinks("stream-p") == 0
	}, "pending source and sink cleaned up")

	h.assertBaseline()
}

// TestExplicitCloseConcurrentWithDisconnect fires an explicit CLOSE and a
// socket disconnect concurrently, many times, asserting the result is always
// consistent: quota released once and never driven negative.
func TestExplicitCloseConcurrentWithDisconnect(t *testing.T) {
	h := newTestHarness(t)

	const iterations = 25

	for i := 0; i < iterations; i++ {
		c := h.dial()
		reqID := "req" + itoa(i)
		sid := "stream-conc" + itoa(i)

		c.publish(reqID, sid)
		c.waitFor("OK", reqID, testTimeout)
		c.waitFor("OFFER", reqID, testTimeout)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.closeRequest(reqID) // Explicit CLOSE
		}()
		go func() {
			defer wg.Done()
			c.shutdown() // Abnormal disconnect
		}()
		wg.Wait()

		h.waitForHandlerGone(c)
		h.waitForSourceGone(sid)
	}

	h.waitForIPBaseline()
	h.assertBaseline()

	// The quota map must never have stored a negative value.
	h.node.mutexIpCount.Lock()
	for ip, n := range h.node.ipCount {
		if n == 0 {
			t.Fatalf("zero entry retained for %s", ip)
		}
	}
	h.node.mutexIpCount.Unlock()
}

// TestPublisherReplacement verifies that a new publisher of the same stream
// closes the old source, returns its sinks to STANDBY (old tracks invalidated),
// and that once the new source is ready the sinks receive the new tracks.
func TestPublisherReplacement(t *testing.T) {
	h := newTestHarness(t)

	// First publisher.
	pub1 := h.dial()
	pub1.publish("p1", "stream-r")
	pub1.waitFor("OK", "p1", testTimeout)
	pub1.waitFor("OFFER", "p1", testTimeout)
	pub1Handler := h.handlerFor(pub1)
	source1 := sourceFor(pub1Handler, "p1")
	h.factory.fakeSource(source1).simulateReady()
	h.waitForSourceReady(source1)

	// Sink attached to the first source's tracks.
	play := h.dial()
	play.play("s1", "stream-r")
	play.waitFor("OK", "s1", testTimeout)
	play.waitFor("STANDBY", "s1", testTimeout)
	play.waitFor("OFFER", "s1", testTimeout)
	playHandler := h.handlerFor(play)
	sink := sinkFor(playHandler, "s1")

	waitForCondition(t, testTimeout, func() bool {
		return h.factory.fakeSink(sink) != nil
	}, "sink peer created")
	oldPeer := h.factory.fakeSink(sink)
	oldVideo, _ := oldPeer.trackIDs()
	if oldVideo == "" {
		t.Fatal("sink never received the original tracks")
	}

	// A second publisher replaces the first.
	pub2 := h.dial()
	pub2.publish("p2", "stream-r")
	pub2.waitFor("OK", "p2", testTimeout)
	pub2.waitFor("OFFER", "p2", testTimeout)
	pub2Handler := h.handlerFor(pub2)
	source2 := sourceFor(pub2Handler, "p2")

	// Old source must be closed and its publish request released.
	waitForCondition(t, testTimeout, func() bool {
		return h.factory.fakeSource(source1).isClosed()
	}, "old source closed")
	pub1.waitFor("CLOSE", "p1", testTimeout)
	if requestTypeOf(pub1Handler, "p1") != 0 {
		t.Fatal("old publisher request bookkeeping not released")
	}
	if requestCountOf(pub1Handler) != 0 {
		t.Fatalf("old publisher requestCount = %d, want 0", requestCountOf(pub1Handler))
	}

	// Exactly one active source for the stream.
	if h.registeredSource("stream-r") != source2 {
		t.Fatal("new source is not the registered source")
	}

	// The sink must have been returned to STANDBY and detached from old tracks.
	play.waitFor("STANDBY", "s1", testTimeout)
	waitForCondition(t, testTimeout, func() bool {
		return oldPeer.isClosed()
	}, "old sink peer closed on replacement")

	// Deterministic check that no new peer was started with the stale tracks
	// while the sink is waiting (the factory still maps the sink to the old,
	// now-closed peer; no replacement has happened yet).
	if fp := h.factory.fakeSink(sink); fp != oldPeer {
		t.Fatal("sink started a new peer before the replacement source was ready")
	}
	sink.statusMutex.Lock()
	if sink.localTrackVideo != nil || sink.localTrackAudio != nil || sink.peer != nil {
		sink.statusMutex.Unlock()
		t.Fatal("sink retained stale tracks/peer while in STANDBY")
	}
	sink.statusMutex.Unlock()

	// New source becomes ready: the sink re-attaches to the NEW tracks.
	h.factory.fakeSource(source2).simulateReady()
	h.waitForSourceReady(source2)
	play.waitFor("OFFER", "s1", testTimeout)

	// Find the sink's newest peer and assert its tracks differ from the old.
	waitForCondition(t, testTimeout, func() bool {
		fp := h.factory.fakeSink(sink)
		if fp == nil || fp == oldPeer {
			return false
		}
		v, _ := fp.trackIDs()
		return v != ""
	}, "sink re-attached with a new peer")
	newPeer := h.factory.fakeSink(sink)
	newVideo, _ := newPeer.trackIDs()
	if newVideo == oldVideo {
		t.Fatalf("sink received the OLD track %q after replacement", newVideo)
	}

	// Old sink must never have been restarted with new tracks: the new peer is
	// a distinct transport instance.
	if newPeer == oldPeer {
		t.Fatal("sink reused the stale peer transport")
	}

	pub2.closeRequest("p2")
	h.waitForSourceGone("stream-r")
	play.closeRequest("s1")
	waitForCondition(t, testTimeout, func() bool {
		return h.registeredSinks("stream-r") == 0
	}, "sink removed")

	pub1.shutdown()
	pub2.shutdown()
	play.shutdown()
	h.waitForHandlerGone(pub1)
	h.waitForHandlerGone(pub2)
	h.waitForHandlerGone(play)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestIPConcurrentLimit enforces the per-IP quota and verifies rejected
// connections do not consume a slot, while closed connections release it.
func TestIPConcurrentLimit(t *testing.T) {
	h := newTestHarness(t)
	h.node.ipLimit = 2

	var clients []*wsClient
	// Fill the quota.
	for i := 0; i < 2; i++ {
		c := h.dial()
		clients = append(clients, c)
		waitForCondition(t, testTimeout, func() bool {
			return h.ipQuota("127.0.0.1") == uint32(i+1)
		}, "quota incremented")
	}

	// A third connection must be rejected (HTTP 429) and not counted.
	_, resp, err := websocket.DefaultDialer.Dial(h.wsURL, nil)
	if err == nil {
		t.Fatal("expected rejection over the IP limit")
	}
	if resp == nil || resp.StatusCode != 429 {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("status = %d, want 429", code)
	}
	if h.ipQuota("127.0.0.1") != 2 {
		t.Fatalf("rejected connection altered the quota: %d", h.ipQuota("127.0.0.1"))
	}

	// Releasing one slot makes room for a new connection.
	clients[0].shutdown()
	h.waitForHandlerGone(clients[0])
	waitForCondition(t, testTimeout, func() bool {
		return h.ipQuota("127.0.0.1") == 1
	}, "quota decremented")

	c := h.dial()
	clients = append(clients[1:], c)
	waitForCondition(t, testTimeout, func() bool {
		return h.ipQuota("127.0.0.1") == 2
	}, "quota refilled")

	for _, cl := range clients {
		cl.shutdown()
	}
	for _, cl := range clients {
		h.waitForHandlerGone(cl)
	}
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestSinkReconnectWhileActive makes the sink transport fail while the source
// stays up; the sink must reconnect with the same tracks and keep its
// registration and request bookkeeping intact.
func TestSinkReconnectWhileActive(t *testing.T) {
	h := newTestHarness(t)

	pub := h.dial()
	pub.publish("p", "stream-rec")
	pub.waitFor("OK", "p", testTimeout)
	pub.waitFor("OFFER", "p", testTimeout)
	pubHandler := h.handlerFor(pub)
	source := sourceFor(pubHandler, "p")
	h.factory.fakeSource(source).simulateReady()
	h.waitForSourceReady(source)

	play := h.dial()
	play.play("s", "stream-rec")
	play.waitFor("OK", "s", testTimeout)
	play.waitFor("STANDBY", "s", testTimeout)
	play.waitFor("OFFER", "s", testTimeout)
	playHandler := h.handlerFor(play)
	sink := sinkFor(playHandler, "s")

	waitForCondition(t, testTimeout, func() bool {
		return h.factory.fakeSink(sink) != nil
	}, "sink peer created")
	firstPeer := h.factory.fakeSink(sink)
	firstVideo, _ := firstPeer.trackIDs()

	// The underlying transport fails; the sink must reconnect.
	firstPeer.simulateDisconnect()
	play.waitFor("OFFER", "s", testTimeout) // Re-offer after reconnect

	waitForCondition(t, testTimeout, func() bool {
		fp := h.factory.fakeSink(sink)
		return fp != nil && fp != firstPeer
	}, "reconnect peer created")
	secondPeer := h.factory.fakeSink(sink)
	secondVideo, _ := secondPeer.trackIDs()
	if secondVideo != firstVideo {
		t.Fatalf("reconnected sink tracks = %q, want same track %q", secondVideo, firstVideo)
	}

	// Registration and per-socket bookkeeping remain intact.
	if h.registeredSinks("stream-rec") != 1 {
		t.Fatal("sink registration lost during reconnect")
	}
	if requestCountOf(playHandler) != 1 {
		t.Fatal("request bookkeeping changed during reconnect")
	}

	play.closeRequest("s")
	pub.closeRequest("p")
	h.waitForSourceGone("stream-rec")
	pub.shutdown()
	play.shutdown()
	h.waitForHandlerGone(pub)
	h.waitForHandlerGone(play)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestSourceTransportFailureReleasesQuota drives the source's own transport
// disconnect (e.g. peer connection failed) path and verifies the request
// bookkeeping, source registry and (on socket close) IP quota are released.
func TestSourceTransportFailureReleasesQuota(t *testing.T) {
	h := newTestHarness(t)

	c := h.dial()
	c.publish("p", "stream-fail")
	c.waitFor("OK", "p", testTimeout)
	c.waitFor("OFFER", "p", testTimeout)
	handler := h.handlerFor(c)
	source := sourceFor(handler, "p")
	h.factory.fakeSource(source).simulateReady()
	h.waitForSourceReady(source)

	// The WebRTC transport fails on its own.
	h.factory.fakeSource(source).simulateDisconnect()

	c.waitFor("CLOSE", "p", testTimeout)
	h.waitForSourceGone("stream-fail")
	waitForCondition(t, testTimeout, func() bool {
		return requestTypeOf(handler, "p") == 0 && requestCountOf(handler) == 0
	}, "request bookkeeping released after transport failure")

	// request-id reusable after the server-initiated CLOSE.
	c.publish("p", "stream-fail-2")
	c.waitFor("OK", "p", testTimeout)
	c.waitFor("OFFER", "p", testTimeout)
	if requestTypeOf(handler, "p") != REQUEST_TYPE_PUBLISH {
		t.Fatal("request-id not reusable after transport failure")
	}

	c.shutdown()
	h.waitForHandlerGone(c)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestDoubleCloseIdempotent invokes the teardown paths repeatedly and asserts
// the per-socket counter never underflows and registries stay consistent.
func TestDoubleCloseIdempotent(t *testing.T) {
	h := newTestHarness(t)

	c := h.dial()
	c.publish("p", "stream-idem")
	c.waitFor("OK", "p", testTimeout)
	c.waitFor("OFFER", "p", testTimeout)
	c.play("s", "stream-idem")
	c.waitFor("OK", "s", testTimeout)
	c.waitFor("STANDBY", "s", testTimeout)

	handler := h.handlerFor(c)

	// Repeated explicit CLOSE on the same ids: only the first acts.
	c.closeRequest("p")
	c.closeRequest("p")
	c.closeRequest("s")
	c.closeRequest("s")

	waitForCondition(t, testTimeout, func() bool {
		return requestCountOf(handler) == 0
	}, "request count back to zero")

	// Run the connection teardown path multiple times concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.onClose()
		}()
	}
	wg.Wait()

	if requestCountOf(handler) != 0 {
		t.Fatalf("requestCount = %d after repeated close, want 0", requestCountOf(handler))
	}

	// Repeated node-level cleanup must not underflow the shared IP counter.
	for i := 0; i < 5; i++ {
		h.node.onConnectionClose(handler.id, handler.ip)
	}
	if h.ipQuota(handler.ip) != 0 {
		t.Fatalf("ip quota = %d after repeated cleanup, want 0", h.ipQuota(handler.ip))
	}

	c.shutdown()
	h.waitForIPBaseline()
	h.assertBaseline()
}

// TestRequestLimitPerSocket verifies the per-socket request cap rejects excess
// requests without altering counters, and that closing one frees a slot.
func TestRequestLimitPerSocket(t *testing.T) {
	h := newTestHarness(t)
	h.node.requestLimit = 2

	c := h.dial()
	defer c.shutdown()

	c.publish("a", "lim-1")
	c.waitFor("OK", "a", testTimeout)
	c.waitFor("OFFER", "a", testTimeout)
	c.play("b", "lim-2")
	c.waitFor("OK", "b", testTimeout)
	c.waitFor("STANDBY", "b", testTimeout)

	handler := h.handlerFor(c)
	if requestCountOf(handler) != 2 {
		t.Fatalf("requestCount = %d, want 2", requestCountOf(handler))
	}

	c.publish("c", "lim-3")
	errMsg := c.waitFor("ERROR", "c", testTimeout)
	if errMsg.params["error-code"] != "LIMIT_REQUESTS" {
		t.Fatalf("error code = %q, want LIMIT_REQUESTS", errMsg.params["error-code"])
	}
	if requestCountOf(handler) != 2 {
		t.Fatal("rejected request altered the per-socket count")
	}

	// Releasing a request frees a slot.
	c.closeRequest("a")
	waitForCondition(t, testTimeout, func() bool {
		return requestCountOf(handler) == 1
	}, "slot freed")

	c.publish("c", "lim-3")
	c.waitFor("OK", "c", testTimeout)
	c.waitFor("OFFER", "c", testTimeout)
	if requestCountOf(handler) != 2 {
		t.Fatalf("requestCount = %d, want 2 after slot reuse", requestCountOf(handler))
	}

	c.shutdown()
	h.waitForHandlerGone(c)
	h.waitForIPBaseline()
	h.assertBaseline()
}

// itoa avoids pulling strconv into every call site signature readability.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
