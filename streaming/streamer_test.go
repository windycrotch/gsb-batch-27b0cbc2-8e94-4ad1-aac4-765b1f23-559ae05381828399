/* Copyright (c) 2024 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package streaming

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onitake/restreamer/api"
	"github.com/onitake/restreamer/auth"
	"github.com/onitake/restreamer/configuration"
	"github.com/onitake/restreamer/metrics"
	"github.com/onitake/restreamer/protocol"
	"github.com/onitake/restreamer/util"
)

// testTimeout is the maximum time a test waits for a condition.
const testTimeout = 5 * time.Second

// testResponseWriter is a concurrency-safe http.ResponseWriter that records
// the status code and the body written to it.
type testResponseWriter struct {
	lock    sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushes int
}

func newTestResponseWriter() *testResponseWriter {
	return &testResponseWriter{
		header: make(http.Header),
		status: 0,
	}
}

func (writer *testResponseWriter) Header() http.Header {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.header
}

func (writer *testResponseWriter) Write(data []byte) (int, error) {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.body.Write(data)
}

func (writer *testResponseWriter) WriteHeader(status int) {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *testResponseWriter) Flush() {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	writer.flushes++
}

func (writer *testResponseWriter) StatusCode() int {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.status
}

func (writer *testResponseWriter) Len() int {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.body.Len()
}

// testBroker is a ConnectionBroker that counts accepts and releases while
// enforcing an optional limit.
type testBroker struct {
	lock     sync.Mutex
	max      int
	active   int
	accepted int32
	released int32
}

func newTestBroker(max int) *testBroker {
	return &testBroker{max: max}
}

func (broker *testBroker) Accept(remoteaddr string, streamer *Streamer) bool {
	broker.lock.Lock()
	defer broker.lock.Unlock()
	if broker.max > 0 && broker.active >= broker.max {
		return false
	}
	broker.active++
	atomic.AddInt32(&broker.accepted, 1)
	return true
}

func (broker *testBroker) Release(streamer *Streamer) {
	broker.lock.Lock()
	if broker.active > 0 {
		broker.active--
	}
	broker.lock.Unlock()
	atomic.AddInt32(&broker.released, 1)
}

func (broker *testBroker) Active() int {
	broker.lock.Lock()
	defer broker.lock.Unlock()
	return broker.active
}

func (broker *testBroker) Accepted() int {
	return int(atomic.LoadInt32(&broker.accepted))
}

func (broker *testBroker) Released() int {
	return int(atomic.LoadInt32(&broker.released))
}

// testCollector is a metrics.Collector that tracks connection accounting.
type testCollector struct {
	lock     sync.Mutex
	added    int
	removed  int
	upstream bool
}

func (collector *testCollector) ConnectionAdded() {
	collector.lock.Lock()
	collector.added++
	collector.lock.Unlock()
}

func (collector *testCollector) ConnectionRemoved() {
	collector.lock.Lock()
	collector.removed++
	collector.lock.Unlock()
}

func (collector *testCollector) PacketReceived()                       {}
func (collector *testCollector) PacketSent()                           {}
func (collector *testCollector) PacketDropped()                        {}
func (collector *testCollector) SourceConnected()                      {}
func (collector *testCollector) SourceDisconnected()                   {}
func (collector *testCollector) StreamDuration(duration time.Duration) {}
func (collector *testCollector) IsUpstreamConnected() bool             { return collector.upstream }

func (collector *testCollector) Added() int {
	collector.lock.Lock()
	defer collector.lock.Unlock()
	return collector.added
}

func (collector *testCollector) Removed() int {
	collector.lock.Lock()
	defer collector.lock.Unlock()
	return collector.removed
}

// waitFor polls a condition until it becomes true or the timeout expires.
func waitFor(t *testing.T, description string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	// final check, to give a last chance after the deadline
	if !cond() {
		t.Fatalf("timeout waiting for %s", description)
	}
}

func testAuthenticator() auth.Authenticator {
	return auth.NewAuthenticator(configuration.Authentication{}, nil)
}

// TestMain disables streaming log output for the whole test binary.
//
// The streamer and connection goroutines log in the background, and the legacy
// proxy test (see proxy_test.go) installs its own logger while it runs.
// Installing a silent logger once here keeps the noisy streaming internals out
// of the test output.
func TestMain(m *testing.M) {
	util.SetGlobalStandardLogger(&util.DummyLogger{})
	logger = &util.DummyLogger{}
	os.Exit(m.Run())
}

// newTestStreamer creates a streamer that logs nothing, so background
// goroutines can never log to a finished test.
func newTestStreamer(t *testing.T, broker ConnectionBroker, collector metrics.Collector) *Streamer {
	t.Helper()
	streamer := NewStreamer("test", 100, broker, testAuthenticator())
	streamer.SetCollector(collector)
	return streamer
}

// startStream starts the stream loop for the given input queue and waits until
// it is running.
//
// The returned stop function closes the input queue (once) and waits for the
// stream loop to end. It is also registered as a test cleanup, so no stream
// goroutine can outlive its test and log into an unrelated test's logger.
func startStream(t *testing.T, streamer *Streamer, queue chan protocol.MpegTsPacket) func() {
	t.Helper()
	// Wait for a previous stream loop to finish before starting a new one, so
	// that a reconnect is modelled the same way the client does it: the old
	// loop is gone and a fresh invocation begins.
	waitFor(t, "previous stream loop to stop", func() bool {
		return !util.LoadBool(&streamer.running)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// An error here (ErrAlreadyRunning) is not fatal for the test, the
		// waitFor below detects a stream that did not come up.
		_ = streamer.Stream(queue)
	}()
	waitFor(t, "stream to start", func() bool {
		return util.LoadBool(&streamer.running)
	})

	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(queue)
			select {
			case <-done:
			case <-time.After(testTimeout):
				t.Errorf("timeout waiting for the stream loop to end")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// viewer is a client connection running ServeHTTP in the background.
type viewer struct {
	writer *testResponseWriter
	done   chan struct{}
}

// newViewer starts a viewer and returns once the handler has finished (for
// refused connections) or the connection has been accepted (for streaming
// connections).
func newViewer(t *testing.T, streamer *Streamer) *viewer {
	t.Helper()
	v := &viewer{
		writer: newTestResponseWriter(),
		done:   make(chan struct{}),
	}
	request := httptest.NewRequest(http.MethodGet, "/stream", nil)
	go func() {
		streamer.ServeHTTP(v.writer, request)
		close(v.done)
	}()
	return v
}

func (v *viewer) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-v.done:
	case <-time.After(testTimeout):
		t.Fatalf("timeout waiting for viewer to finish")
	}
}

func (v *viewer) isDone() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

// newPacket creates a packet of the standard MPEG-TS size.
func newPacket() protocol.MpegTsPacket {
	packet := make(protocol.MpegTsPacket, protocol.MpegTsPacketSize)
	packet[0] = protocol.MpegTsSyncByte
	return packet
}

// TestStreamerOfflineOnlineStateSwitch verifies the complete offline/online
// cycle: a viewer is accepted, terminated in an orderly fashion when the
// stream goes offline, its quota is returned, new viewers are refused while
// offline, and new viewers are accepted again after the stream comes back
// online.
func TestStreamerOfflineOnlineStateSwitch(t *testing.T) {
	broker := newTestBroker(0)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue := make(chan protocol.MpegTsPacket, 10)
	stop := startStream(t, streamer, queue)

	// connect one viewer
	v1 := newViewer(t, streamer)
	waitFor(t, "first viewer accepted", func() bool { return broker.Active() == 1 })

	// make sure it actually receives data
	queue <- newPacket()
	waitFor(t, "first viewer to receive a packet", func() bool {
		return v1.writer.Len() >= protocol.MpegTsPacketSize
	})

	// go offline: the viewer must be ended in an orderly fashion
	streamer.SetInhibit(true)

	v1.waitDone(t)
	if status := v1.writer.StatusCode(); status != http.StatusOK {
		t.Errorf("expected a normal stream response (200), got %d", status)
	}
	if broker.Active() != 0 {
		t.Errorf("quota not returned after going offline: active=%d", broker.Active())
	}
	if broker.Released() != 1 {
		t.Errorf("expected exactly one quota release, got %d", broker.Released())
	}
	if collector.Removed() != 1 {
		t.Errorf("expected exactly one connection removal, got %d", collector.Removed())
	}

	// no new viewer is accepted while offline
	v2 := newViewer(t, streamer)
	v2.waitDone(t)
	if status := v2.writer.StatusCode(); status != http.StatusNotFound {
		t.Errorf("expected refusal (404) while offline, got %d", status)
	}
	if broker.Active() != 0 {
		t.Errorf("offline stream accepted a connection: active=%d", broker.Active())
	}

	// go online again: a new viewer is accepted and receives data
	streamer.SetInhibit(false)
	v3 := newViewer(t, streamer)
	waitFor(t, "viewer accepted after coming back online", func() bool { return broker.Active() == 1 })

	queue <- newPacket()
	waitFor(t, "viewer to receive a packet after coming back online", func() bool {
		return v3.writer.Len() >= protocol.MpegTsPacketSize
	})
	if v3.writer.StatusCode() != http.StatusOK {
		t.Errorf("expected 200 for an accepted viewer, got %d", v3.writer.StatusCode())
	}

	// tear down
	streamer.SetInhibit(true)
	v3.waitDone(t)
	stop()

	if broker.Active() != 0 {
		t.Errorf("connection leak after teardown: active=%d", broker.Active())
	}
	if broker.Released() != broker.Accepted() {
		t.Errorf("exactly one release per accept expected: accepted=%d released=%d", broker.Accepted(), broker.Released())
	}
	if collector.Added() != collector.Removed() {
		t.Errorf("connection accounting mismatch: added=%d removed=%d", collector.Added(), collector.Removed())
	}
}

// TestStreamerQuotaRestoredAfterOffline verifies that going offline and
// online restores the full connection quota, using a real AccessController.
func TestStreamerQuotaRestoredAfterOffline(t *testing.T) {
	broker := NewAccessController(2)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue := make(chan protocol.MpegTsPacket, 10)
	stop := startStream(t, streamer, queue)

	// fill one slot
	v1 := newViewer(t, streamer)
	waitFor(t, "first viewer accepted", func() bool {
		return collector.Added() == 1
	})

	// offline must release the slot
	streamer.SetInhibit(true)
	v1.waitDone(t)
	waitFor(t, "quota released after offline", func() bool {
		return collector.Removed() == 1
	})

	// online again: two fresh viewers must fit into the limit
	streamer.SetInhibit(false)
	v2 := newViewer(t, streamer)
	v3 := newViewer(t, streamer)
	waitFor(t, "two viewers accepted after going online again", func() bool {
		return collector.Added() == 3
	})
	time.Sleep(20 * time.Millisecond)
	if v2.isDone() || v3.isDone() {
		t.Fatalf("viewers were refused after recovering from offline")
	}

	// and the limit is still enforced
	v4 := newViewer(t, streamer)
	v4.waitDone(t)
	if status := v4.writer.StatusCode(); status != http.StatusNotFound {
		t.Errorf("expected 404 when the quota is full, got %d", status)
	}

	streamer.SetInhibit(true)
	v2.waitDone(t)
	v3.waitDone(t)
	stop()
}

// TestStreamerUpstreamReconnect verifies that an upstream disconnect and
// reconnect terminates existing viewers exactly once, does not close the same
// queue twice, and that new viewers receive data after the reconnect.
func TestStreamerUpstreamReconnect(t *testing.T) {
	broker := newTestBroker(0)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue1 := make(chan protocol.MpegTsPacket, 10)
	stop1 := startStream(t, streamer, queue1)

	v1 := newViewer(t, streamer)
	waitFor(t, "viewer accepted before reconnect", func() bool { return broker.Active() == 1 })
	queue1 <- newPacket()
	waitFor(t, "viewer to receive data before reconnect", func() bool {
		return v1.writer.Len() >= protocol.MpegTsPacketSize
	})

	// upstream loss: the client closes its input queue
	stop1()
	v1.waitDone(t)
	waitFor(t, "quota released after upstream loss", func() bool { return broker.Active() == 0 })
	if broker.Released() != 1 {
		t.Errorf("expected exactly one release after upstream loss, got %d", broker.Released())
	}

	// upstream reconnect: the client starts a new stream with a new queue
	queue2 := make(chan protocol.MpegTsPacket, 10)
	stop2 := startStream(t, streamer, queue2)

	v2 := newViewer(t, streamer)
	waitFor(t, "viewer accepted after reconnect", func() bool { return broker.Active() == 1 })
	queue2 <- newPacket()
	waitFor(t, "viewer to receive data after reconnect", func() bool {
		return v2.writer.Len() >= protocol.MpegTsPacketSize
	})

	streamer.SetInhibit(true)
	v2.waitDone(t)
	stop2()

	if broker.Released() != broker.Accepted() {
		t.Errorf("expected one release per accept: accepted=%d released=%d", broker.Accepted(), broker.Released())
	}
	if collector.Added() != collector.Removed() {
		t.Errorf("connection accounting mismatch: added=%d removed=%d", collector.Added(), collector.Removed())
	}
}

// TestStreamerOfflineSurvivesReconnect verifies that the offline state is
// persistent across an upstream reconnect.
func TestStreamerOfflineSurvivesReconnect(t *testing.T) {
	broker := newTestBroker(0)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue1 := make(chan protocol.MpegTsPacket, 10)
	stop1 := startStream(t, streamer, queue1)

	streamer.SetInhibit(true)
	stop1()
	waitFor(t, "stream loop to stop", func() bool { return !util.LoadBool(&streamer.running) })

	queue2 := make(chan protocol.MpegTsPacket, 10)
	stop2 := startStream(t, streamer, queue2)

	v := newViewer(t, streamer)
	v.waitDone(t)
	if status := v.writer.StatusCode(); status != http.StatusNotFound {
		t.Errorf("expected 404 after reconnect while offline, got %d", status)
	}
	if broker.Active() != 0 {
		t.Errorf("offline stream accepted a connection after reconnect: active=%d", broker.Active())
	}

	streamer.SetInhibit(false)
	v2 := newViewer(t, streamer)
	waitFor(t, "viewer accepted after going online", func() bool { return broker.Active() == 1 })
	streamer.SetInhibit(true)
	v2.waitDone(t)
	stop2()
}

// TestStreamerConcurrentConnectionsDuringSwitch exercises racing connection
// attempts while the stream repeatedly goes offline and online. Every accepted
// connection must be released exactly once and no connection may be accepted
// while the stream is offline.
func TestStreamerConcurrentConnectionsDuringSwitch(t *testing.T) {
	broker := newTestBroker(0)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue := make(chan protocol.MpegTsPacket, 100)
	stop := startStream(t, streamer, queue)

	const viewers = 40
	writers := make([]*testResponseWriter, viewers)
	done := make([]chan struct{}, viewers)
	for i := 0; i < viewers; i++ {
		writers[i] = newTestResponseWriter()
		done[i] = make(chan struct{})
		request := httptest.NewRequest(http.MethodGet, "/stream", nil)
		writer := writers[i]
		finished := done[i]
		go func() {
			streamer.ServeHTTP(writer, request)
			close(finished)
		}()
	}

	// Let some of them through, then switch offline while connections are
	// still racing in.
	waitFor(t, "some viewers to be accepted", func() bool { return broker.Active() > 0 })
	streamer.SetInhibit(true)

	for i := 0; i < viewers; i++ {
		select {
		case <-done[i]:
		case <-time.After(testTimeout):
			t.Fatalf("viewer %d did not finish after going offline", i)
		}
	}

	if broker.Active() != 0 {
		t.Errorf("connections left after going offline: active=%d", broker.Active())
	}
	if broker.Released() != broker.Accepted() {
		t.Errorf("expected one release per accept: accepted=%d released=%d", broker.Accepted(), broker.Released())
	}
	if collector.Added() != collector.Removed() {
		t.Errorf("connection accounting mismatch: added=%d removed=%d", collector.Added(), collector.Removed())
	}
	if collector.Added() != broker.Accepted() {
		t.Errorf("statistics and broker disagree: added=%d accepted=%d", collector.Added(), broker.Accepted())
	}

	// Every viewer that reported 200 must have been accepted, and every
	// refusal must be a 404.
	ok200 := 0
	notfound := 0
	for _, writer := range writers {
		switch writer.StatusCode() {
		case http.StatusOK:
			ok200++
		case http.StatusNotFound:
			notfound++
		default:
			t.Errorf("unexpected status code %d", writer.StatusCode())
		}
	}
	if ok200 != broker.Accepted() {
		t.Errorf("200 responses (%d) do not match accepted connections (%d)", ok200, broker.Accepted())
	}
	if ok200+notfound != viewers {
		t.Errorf("not all viewers reported a status: 200=%d 404=%d", ok200, notfound)
	}

	streamer.SetInhibit(false)
	stop()
}

// TestConnectionCloseIdempotent verifies that the connection queue can be
// closed concurrently from multiple goroutines while packets are being sent,
// without panicking and with only one effective close.
func TestConnectionCloseIdempotent(t *testing.T) {
	conn := NewConnection(newTestResponseWriter(), 4, "test", nil)

	var wg sync.WaitGroup
	// racers that try to send packets
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if result := conn.trySend(newPacket()); result == sendClosed {
					return
				}
			}
		}()
	}
	// racers that close the queue
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn.closeQueue()
		}()
	}
	wg.Wait()

	if !conn.Closed {
		t.Error("connection should be marked closed")
	}
	// closing again must not panic
	conn.closeQueue()

	// a send after close must report the closed state, not block or panic
	if result := conn.trySend(newPacket()); result != sendClosed {
		t.Errorf("expected sendClosed after shutdown, got %v", result)
	}
}

// TestConnectionServeEndsNormallyOnClose verifies that a connection whose
// queue is closed terminates with a normal HTTP response rather than an
// error.
func TestConnectionServeEndsNormallyOnClose(t *testing.T) {
	writer := newTestResponseWriter()
	conn := NewConnection(writer, 4, "test", httptest.NewRequest(http.MethodGet, "/", nil).Context())

	done := make(chan struct{})
	go func() {
		conn.Serve(nil)
		close(done)
	}()

	// feed a few packets
	for i := 0; i < 3; i++ {
		conn.trySend(newPacket())
	}
	waitFor(t, "packets to be written", func() bool {
		return writer.Len() >= 2*protocol.MpegTsPacketSize
	})

	conn.closeQueue()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return after the queue was closed")
	}
	if writer.StatusCode() != http.StatusOK {
		t.Errorf("expected 200, got %d", writer.StatusCode())
	}
}

// TestStreamerOverlappingReconnect verifies that a new upstream stream can be
// started while the previous stream loop is still winding down. The new loop
// must not be rejected with ErrAlreadyRunning, and viewers connected after the
// reconnect must receive data.
func TestStreamerOverlappingReconnect(t *testing.T) {
	broker := newTestBroker(0)
	collector := &testCollector{}
	streamer := newTestStreamer(t, broker, collector)

	queue1 := make(chan protocol.MpegTsPacket, 10)
	stop1 := startStream(t, streamer, queue1)

	v1 := newViewer(t, streamer)
	waitFor(t, "viewer accepted", func() bool { return broker.Active() == 1 })

	// Simulate an abrupt upstream loss and immediate reconnect: close the old
	// queue and start the new stream without waiting for the old loop to stop.
	stop1()
	queue2 := make(chan protocol.MpegTsPacket, 10)
	errs := make(chan error, 1)
	go func() {
		errs <- streamer.Stream(queue2)
	}()

	v1.waitDone(t)

	waitFor(t, "new stream loop to run", func() bool {
		return util.LoadBool(&streamer.running)
	})
	select {
	case err := <-errs:
		t.Fatalf("new stream loop terminated unexpectedly: %v", err)
	case <-time.After(10 * time.Millisecond):
		// still running, which is what we want
	}

	v2 := newViewer(t, streamer)
	waitFor(t, "viewer accepted after overlapping reconnect", func() bool { return broker.Active() == 1 })
	queue2 <- newPacket()
	waitFor(t, "viewer to receive data after overlapping reconnect", func() bool {
		return v2.writer.Len() >= protocol.MpegTsPacketSize
	})

	streamer.SetInhibit(true)
	v2.waitDone(t)
	close(queue2)
	select {
	case err := <-errs:
		if err != nil {
			t.Errorf("stream loop returned an error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for the overlapping stream loop to end")
	}

	if broker.Released() != broker.Accepted() {
		t.Errorf("expected one release per accept: accepted=%d released=%d", broker.Accepted(), broker.Released())
	}
	if collector.Added() != collector.Removed() {
		t.Errorf("connection accounting mismatch: added=%d removed=%d", collector.Added(), collector.Removed())
	}
}

// healthViewerCount queries the health API and returns the reported viewer
// count.
func healthViewerCount(t *testing.T, stats metrics.Statistics) int {
	t.Helper()
	handler := api.NewHealthApi(stats, testAuthenticator())
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(writer, request)
	var decoded struct {
		Viewer int `json:"viewer"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("error decoding health response: %s", err)
	}
	return decoded.Viewer
}

// TestHealthReportsOnlyLiveViewers verifies that the health API reports the
// number of viewers that are actually online, and that viewers ended by an
// offline switch are no longer counted.
func TestHealthReportsOnlyLiveViewers(t *testing.T) {
	stats := metrics.NewStatistics(0, 0)
	collector := stats.RegisterStream("health")
	streamer := NewStreamer("health", 100, newTestBroker(0), testAuthenticator())
	streamer.SetCollector(collector)

	queue := make(chan protocol.MpegTsPacket, 10)
	stop := startStream(t, streamer, queue)

	if got := healthViewerCount(t, stats); got != 0 {
		t.Fatalf("expected 0 viewers before any connection, got %d", got)
	}

	v1 := newViewer(t, streamer)
	v2 := newViewer(t, streamer)
	waitFor(t, "both viewers to be counted", func() bool {
		return healthViewerCount(t, stats) == 2
	})

	// going offline ends both viewers and they must disappear from the count
	streamer.SetInhibit(true)
	v1.waitDone(t)
	v2.waitDone(t)
	if got := healthViewerCount(t, stats); got != 0 {
		t.Errorf("expected 0 viewers after going offline, got %d", got)
	}

	// back online: only the new viewer is counted
	streamer.SetInhibit(false)
	v3 := newViewer(t, streamer)
	waitFor(t, "new viewer to be counted", func() bool {
		return healthViewerCount(t, stats) == 1
	})

	streamer.SetInhibit(true)
	v3.waitDone(t)
	stop()
	if got := healthViewerCount(t, stats); got != 0 {
		t.Errorf("expected 0 viewers after final offline, got %d", got)
	}
}

// TestSetInhibitWithoutStream verifies that the offline flag can be toggled
// while no stream loop is running, and that it takes effect once a stream
// starts.
func TestSetInhibitWithoutStream(t *testing.T) {
	broker := newTestBroker(0)
	streamer := newTestStreamer(t, broker, &testCollector{})

	// no stream running: must not block
	done := make(chan struct{})
	go func() {
		streamer.SetInhibit(true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("SetInhibit blocked without a running stream")
	}
	if !streamer.isInhibited() {
		t.Fatal("offline flag not set without a running stream")
	}

	queue := make(chan protocol.MpegTsPacket, 10)
	stop := startStream(t, streamer, queue)

	v := newViewer(t, streamer)
	v.waitDone(t)
	if status := v.writer.StatusCode(); status != http.StatusNotFound {
		t.Errorf("expected 404 for a stream that was set offline before it started, got %d", status)
	}

	streamer.SetInhibit(false)
	streamer.SetInhibit(true)
	stop()
}
