package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testHarness owns a node running its real HTTP/websocket signaling entry
// point on a loopback server, backed by in-memory peer transports. No Redis
// (standalone mode) and no external WebRTC peer are required.
type testHarness struct {
	t       *testing.T
	node    *WebRTC_CDN_Node
	server  *httptest.Server
	factory *fakeTransportFactory
	wsURL   string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	// Standalone: redis sends are no-ops and no listener is required.
	t.Setenv("STAND_ALONE", "YES")
	t.Setenv("JWT_SECRET", "") // Authentication disabled
	t.Setenv("LOG_REQUESTS", "NO")
	t.Setenv("LOG_DEBUG", "NO")

	node := &WebRTC_CDN_Node{id: "test-node"}
	factory := newFakeTransportFactory()
	node.newSourcePeer = factory.sourceFactory()
	node.newSinkPeer = factory.sinkFactory()
	node.init()
	node.upgrader = &websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	server := httptest.NewServer(node)

	t.Cleanup(func() {
		server.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	return &testHarness{
		t:       t,
		node:    node,
		server:  server,
		factory: factory,
		wsURL:   wsURL,
	}
}

// dial opens a new websocket signaling connection from the test client. Every
// connection in a test shares 127.0.0.1, which is exactly what the per-IP
// concurrency quota counts.
func (h *testHarness) dial() *wsClient {
	h.t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(h.wsURL, nil)
	if err != nil {
		h.t.Fatalf("dial failed: %v", err)
	}

	conn.SetCloseHandler(func(code int, text string) error { return nil })

	return &wsClient{t: h.t, conn: conn}
}

// wsClient is a thin signaling client wrapping a websocket connection.
type wsClient struct {
	t       *testing.T
	conn    *websocket.Conn
	readMu  sync.Mutex
	pending []SignalingMessage
	pendMu  sync.Mutex
}

func (c *wsClient) sendRaw(raw string) {
	c.t.Helper()
	// A write can fail if the server already tore the socket down (expected in
	// disconnect scenarios). Report it as a soft error instead of failing the
	// whole test; the scenario assertions cover the resulting state.
	if err := c.conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		c.t.Logf("write failed (socket likely closed): %v", err)
	}
}

func (c *wsClient) publish(requestID, streamID string) {
	c.sendRaw("PUBLISH\nrequest-id:" + requestID + "\nstream-id:" + streamID + "\nstream-type:VIDEO\n")
}

func (c *wsClient) play(requestID, streamID string) {
	c.sendRaw("PLAY\nrequest-id:" + requestID + "\nstream-id:" + streamID + "\n")
}

func (c *wsClient) closeRequest(requestID string) {
	c.sendRaw("CLOSE\nrequest-id:" + requestID + "\n")
}

func (c *wsClient) answer(requestID, body string) {
	c.sendRaw("ANSWER\nrequest-id:" + requestID + "\n\n" + body + "\n")
}

func (c *wsClient) candidate(requestID, body string) {
	c.sendRaw("CANDIDATE\nrequest-id:" + requestID + "\n\n" + body + "\n")
}

func (c *wsClient) heartbeat() {
	c.sendRaw("HEARTBEAT\n")
}

func (c *wsClient) shutdown() {
	_ = c.conn.Close()
}

// readOne reads the next signaling message, waiting up to the timeout.
func (c *wsClient) readOne(timeout time.Duration) (SignalingMessage, bool) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := c.conn.ReadMessage()
	if err != nil {
		return SignalingMessage{}, false
	}
	_ = c.conn.SetReadDeadline(time.Time{})

	return parseSignalingMessage(string(data)), true
}

// waitFor blocks until a message with the given method (and request-id when
// non-empty) arrives, failing the test on timeout. Unrelated messages are
// buffered so later waits still see them.
func (c *wsClient) waitFor(method, requestID string, timeout time.Duration) SignalingMessage {
	c.t.Helper()

	// Check the buffer first.
	c.pendMu.Lock()
	for i, m := range c.pending {
		if m.method == method && (requestID == "" || m.params["request-id"] == requestID) {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.pendMu.Unlock()
			return m
		}
	}
	c.pendMu.Unlock()

	deadline := time.Now().Add(timeout)
	for {
		msg, ok := c.readOne(time.Until(deadline))
		if !ok {
			c.t.Fatalf("timed out waiting for %s (request %s)", method, requestID)
		}
		if msg.method == method && (requestID == "" || msg.params["request-id"] == requestID) {
			return msg
		}
		c.pendMu.Lock()
		c.pending = append(c.pending, msg)
		c.pendMu.Unlock()
	}
}

// expectNone asserts no matching message arrives within the timeout.
func (c *wsClient) expectNone(method, requestID string, timeout time.Duration) {
	c.t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		msg, ok := c.readOne(remaining)
		if !ok {
			return
		}
		if msg.method == method && (requestID == "" || msg.params["request-id"] == requestID) {
			c.t.Fatalf("unexpected message %s for request %s", method, requestID)
		}
		c.pendMu.Lock()
		c.pending = append(c.pending, msg)
		c.pendMu.Unlock()
	}
}

// drainAll reads every currently-available message (non-blocking overall).
func (c *wsClient) drainAll() []SignalingMessage {
	c.t.Helper()

	var out []SignalingMessage
	for {
		msg, ok := c.readOne(100 * time.Millisecond)
		if !ok {
			break
		}
		out = append(out, msg)
	}
	return out
}

// waitForCondition polls until cond() is true or the timeout elapses.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, description string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !cond() {
		t.Fatalf("condition not met within %s: %s", timeout, description)
	}
}

// remotePort returns the client's ephemeral source port. On the client side
// that is the *local* address of its socket; the server records the same port
// as the socket's remote port (handler.port), so it uniquely identifies the
// connection end-to-end on loopback.
func (c *wsClient) remotePort() string {
	if addr, ok := c.conn.LocalAddr().(*net.TCPAddr); ok {
		return fmt.Sprintf("%d", addr.Port)
	}
	_, port, err := net.SplitHostPort(c.conn.LocalAddr().String())
	if err != nil {
		c.t.Fatalf("cannot resolve local port: %v", err)
	}
	return port
}

// handlerFor maps a client connection to its server-side handler. Every
// accepted server-side socket shares the listener's local port, so that is
// not unique; the client's ephemeral port is unique and appears on the server
// side as the socket's *remote* port. It waits for registration, which happens
// in a goroutine after the websocket upgrade completes.
func (h *testHarness) handlerFor(c *wsClient) *Connection_Handler {
	h.t.Helper()

	clientPort := c.remotePort()

	var found *Connection_Handler
	waitForCondition(h.t, testTimeout, func() bool {
		h.node.mutexConnections.Lock()
		defer h.node.mutexConnections.Unlock()
		for _, handler := range h.node.connections {
			if handler.port == clientPort {
				found = handler
				return true
			}
		}
		return false
	}, "server-side handler registered for client port "+clientPort)

	return found
}

// assertBaseline verifies every quota and registry has returned to its empty
// baseline. It is the core invariant checked at the end of every scenario.
func (h *testHarness) assertBaseline() {
	h.t.Helper()

	h.node.mutexConnections.Lock()
	connCount := len(h.node.connections)
	h.node.mutexConnections.Unlock()

	h.node.mutexIpCount.Lock()
	ipMap := make(map[string]uint32, len(h.node.ipCount))
	for k, v := range h.node.ipCount {
		ipMap[k] = v
	}
	h.node.mutexIpCount.Unlock()

	h.node.mutexStatus.Lock()
	sourceCount := len(h.node.sources)
	sinkCount := 0
	for _, m := range h.node.sinks {
		sinkCount += len(m)
	}
	relayCount := len(h.node.relays)
	senderCount := len(h.node.senders)
	h.node.mutexStatus.Unlock()

	if connCount != 0 {
		h.t.Errorf("baseline: %d connections still registered", connCount)
	}
	if len(ipMap) != 0 {
		h.t.Errorf("baseline: per-IP quota map not empty: %v", ipMap)
	}
	if sourceCount != 0 {
		h.t.Errorf("baseline: %d sources still registered", sourceCount)
	}
	if sinkCount != 0 {
		h.t.Errorf("baseline: %d sinks still registered", sinkCount)
	}
	if relayCount != 0 {
		h.t.Errorf("baseline: %d relays still registered", relayCount)
	}
	if senderCount != 0 {
		h.t.Errorf("baseline: %d senders still registered", senderCount)
	}
}

// ipQuota returns the current per-IP connection count for the given IP.
func (h *testHarness) ipQuota(ip string) uint32 {
	h.node.mutexIpCount.Lock()
	defer h.node.mutexIpCount.Unlock()

	return h.node.ipCount[ip]
}

func requestCountOf(handler *Connection_Handler) uint32 {
	handler.statusMutex.Lock()
	defer handler.statusMutex.Unlock()

	return handler.requestCount
}

func requestTypeOf(handler *Connection_Handler, requestID string) int {
	handler.statusMutex.Lock()
	defer handler.statusMutex.Unlock()

	return handler.requests[requestID]
}

func sourceFor(handler *Connection_Handler, requestID string) *WRTC_Source {
	handler.statusMutex.Lock()
	defer handler.statusMutex.Unlock()

	return handler.sources[requestID]
}

func sinkFor(handler *Connection_Handler, requestID string) *WRTC_Sink {
	handler.statusMutex.Lock()
	defer handler.statusMutex.Unlock()

	return handler.sinks[requestID]
}
