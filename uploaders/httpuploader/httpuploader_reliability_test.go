package httpuploader

import (
	"errors"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
)

// newTestUploader builds an uploader pointing at the test server with a
// short backoff so retry tests stay fast and deterministic.
func newTestUploader(t *testing.T, server *httptest.Server, maxRetries int) HTTPUploader {
	u, errURL := url.Parse(server.URL)
	if errURL != nil {
		t.Fatalf("Error parsing test server URL. Err %v", errURL)
	}
	return New(nil, false, u.Scheme, u.Host, maxRetries, 1)
}

// record keeps track of what a flaky origin received.
type record struct {
	mu          sync.Mutex
	statusCodes []int
	bodies      [][]byte
}

func (r *record) add(status int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusCodes = append(r.statusCodes, status)
	bodyCopy := make([]byte, len(body))
	copy(bodyCopy, body)
	r.bodies = append(r.bodies, bodyCopy)
}

func (r *record) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.statusCodes)
}

// flakyServer returns statusCode for the first failTimes requests and 200
// afterwards. It records every request body so retry payloads can be
// compared.
func flakyServer(t *testing.T, rec *record, failTimes int32, statusCode int) *httptest.Server {
	var failures int32
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, errRead := ioutil.ReadAll(req.Body)
		if errRead != nil {
			t.Error("Error reading request body. Err: ", errRead)
		}

		attempt := atomic.AddInt32(&failures, 1)
		if attempt <= failTimes {
			rec.add(statusCode, body)
			rw.WriteHeader(statusCode)
			return
		}
		rec.add(http.StatusOK, body)
		rw.Write([]byte("OK"))
	}))
}

// TestUploadDataRetryThenSuccess: a 503 followed by a success must be
// retried with backoff and eventually return nil. This is the
// "retryable failure then success" scenario.
func TestUploadDataRetryThenSuccess(t *testing.T) {
	data := []byte("retry-me-please")
	rec := &record{}
	server := flakyServer(t, rec, 2, http.StatusServiceUnavailable)
	defer server.Close()

	up := newTestUploader(t, server, 5)

	if err := up.UploadData(data, "test/retry.ts", nil); err != nil {
		t.Fatalf("Expected upload to succeed after retries, got error: %v", err)
	}

	if got := rec.count(); got != 3 {
		t.Errorf("Expected 3 attempts (2 failures + 1 success), got %d", got)
	}

	// Every retry must resend the WHOLE payload. Before the fix retries
	// re-used an already consumed reader and uploaded empty bodies.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, body := range rec.bodies {
		if !testBinary(body, data) {
			t.Errorf("Attempt %d sent %d bytes, want full payload of %d bytes", i+1, len(body), len(data))
		}
	}
}

// TestUploadLocalFileRetryThenSuccess checks the file-based path also
// replays the complete file on each attempt.
func TestUploadLocalFileRetryThenSuccess(t *testing.T) {
	testFilePath := "../../fixture/testSmall.ts"
	rec := &record{}
	server := flakyServer(t, rec, 1, http.StatusServiceUnavailable)
	defer server.Close()

	up := newTestUploader(t, server, 4)

	expected, errRead := ioutil.ReadFile(testFilePath)
	if errRead != nil {
		t.Fatalf("Error reading expected file: %v", errRead)
	}

	if err := up.UploadLocalFile(testFilePath, "test/retry-file.ts", nil); err != nil {
		t.Fatalf("Expected upload to succeed after retry, got error: %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Errorf("Expected 2 attempts, got %d", got)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, body := range rec.bodies {
		if !testBinary(body, expected) {
			t.Errorf("Attempt %d sent %d bytes, want %d bytes", i+1, len(body), len(expected))
		}
	}
}

// TestUploadDataRetriesExhausted: when the origin keeps failing the
// uploader must give up after MaxHTTPRetries attempts and return a
// non-nil error wrapping ErrRetriesExhausted instead of returning nil and
// silently losing the data.
func TestUploadDataRetriesExhausted(t *testing.T) {
	data := []byte("never-arrives")
	rec := &record{}
	server := flakyServer(t, rec, 1000, http.StatusServiceUnavailable)
	defer server.Close()

	const maxRetries = 3
	up := newTestUploader(t, server, maxRetries)

	err := up.UploadData(data, "test/fail.ts", nil)
	if err == nil {
		t.Fatal("Expected an error after retries exhausted, got nil")
	}
	if !errors.Is(err, uploaders.ErrRetriesExhausted) {
		t.Errorf("Expected ErrRetriesExhausted, got: %v", err)
	}

	if got := rec.count(); got != maxRetries {
		t.Errorf("Expected exactly %d attempts, got %d", maxRetries, got)
	}
}

// TestUploadDataPermanentErrorNotRetried: a 400 cannot be fixed by
// retrying; it must fail immediately and wrap ErrPermanentUploadError.
func TestUploadDataPermanentErrorNotRetried(t *testing.T) {
	data := []byte("bad-request")
	rec := &record{}
	server := flakyServer(t, rec, 1000, http.StatusBadRequest)
	defer server.Close()

	up := newTestUploader(t, server, 5)

	err := up.UploadData(data, "test/bad.ts", nil)
	if err == nil {
		t.Fatal("Expected a permanent error, got nil")
	}
	if !errors.Is(err, uploaders.ErrPermanentUploadError) {
		t.Errorf("Expected ErrPermanentUploadError, got: %v", err)
	}

	if got := rec.count(); got != 1 {
		t.Errorf("Permanent error must not be retried, expected 1 attempt, got %d", got)
	}
}

// TestUploadDataTransportErrorRetried: a connection-level failure (origin
// closing the connection without a response) is retryable and must
// succeed once the origin answers again.
func TestUploadDataTransportErrorRetried(t *testing.T) {
	data := []byte("transport-retry")
	var mu sync.Mutex
	received := make([][]byte, 0)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := ioutil.ReadAll(req.Body)
		mu.Lock()
		attempt := len(received)
		received = append(received, body)
		mu.Unlock()

		if attempt == 0 {
			// Hijack and close the TCP connection: the client sees a
			// transport error, not an HTTP status.
			hj, ok := rw.(http.Hijacker)
			if !ok {
				t.Error("httptest server does not support hijacking")
				return
			}
			conn, _, errHijack := hj.Hijack()
			if errHijack != nil {
				t.Error("Error hijacking connection: ", errHijack)
				return
			}
			conn.Close()
			return
		}
		rw.Write([]byte("OK"))
	}))
	defer server.Close()

	up := newTestUploader(t, server, 4)

	if err := up.UploadData(data, "test/transport.ts", nil); err != nil {
		t.Fatalf("Expected success after transport error retry, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("Expected 2 connections, got %d", len(received))
	}
	for i, body := range received {
		if !testBinary(body, data) {
			t.Errorf("Connection %d sent %d bytes, want %d", i+1, len(body), len(data))
		}
	}
}

// trackingListener wraps a listener so the test can forcibly close every
// accepted (and possibly hanging) connection during teardown. This keeps
// the timeout test portable: httptest.Server.Close waits for active
// connections, which on Windows never notice a client that timed out
// while the request body was still in flight.
type trackingListener struct {
	net.Listener
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func newTrackingListener(inner net.Listener) *trackingListener {
	return &trackingListener{Listener: inner, conns: make(map[net.Conn]struct{})}
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		c.Close()
		return c, nil
	}
	// Keep every accepted connection in the map until closeAll: normal
	// request completion closes the conn but must NOT remove it, otherwise
	// a later hanging connection would be unreachable during teardown.
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return &trackingConn{Conn: c}, nil
}

func (l *trackingListener) closeAll() {
	l.mu.Lock()
	l.closed = true
	for c := range l.conns {
		_ = c.Close()
	}
	l.conns = make(map[net.Conn]struct{})
	l.mu.Unlock()
}

type trackingConn struct {
	net.Conn
}

// newHangingTestServer builds an httptest server whose first request is
// never answered (forcing a client timeout). The returned release func
// unblocks hanging handlers, and the listener can forcibly drop every
// connection, so teardown never waits on a dead client connection.
func newHangingTestServer(t *testing.T) (server *httptest.Server, listener *trackingListener, attempts *int32, release func()) {
	var attemptCount int32
	attempts = &attemptCount
	hang := make(chan struct{})
	release = func() { close(hang) }

	baseListener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("Error creating listener: %v", errListen)
	}
	listener = newTrackingListener(baseListener)

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if atomic.AddInt32(&attemptCount, 1) == 1 {
			// Never answer until the test explicitly releases us.
			<-hang
			return
		}
		rw.Write([]byte("OK"))
	})

	server = &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server, listener, attempts, release
}

// TestUploadDataTimeoutRetried: a server that never responds triggers the
// client timeout, which must be treated as retryable and followed by a
// successful attempt.
func TestUploadDataTimeoutRetried(t *testing.T) {
	data := []byte("slow-origin")
	server, listener, attempts, release := newHangingTestServer(t)
	defer func() {
		// First let the hanging handler return, then drop connections and
		// stop the server so httptest does not wait on a dead client.
		release()
		listener.closeAll()
		server.Close()
	}()

	u, errURL := url.Parse(server.URL)
	if errURL != nil {
		t.Fatalf("Error parsing test server URL. Err %v", errURL)
	}
	up := New(nil, false, u.Scheme, u.Host, 3, 1)
	up.HTTPClient.Timeout = 100 * time.Millisecond

	if err := up.UploadData(data, "test/timeout.ts", nil); err != nil {
		t.Fatalf("Expected success after timeout retry, got: %v", err)
	}

	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Errorf("Expected 2 attempts, got %d", got)
	}
}

// TestUploadChunkedTransferFailureObservable: chunked transfers cannot be
// retried, but a 500 from the origin must be surfaced on the error
// channel instead of being swallowed.
func TestUploadChunkedTransferFailureObservable(t *testing.T) {
	uploadFilePath := "test/chunked-fail.ts"
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = ioutil.ReadAll(req.Body)
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	up := newTestUploader(t, server, 2)

	writeChan, errChan := up.UploadChunkedTransfer(uploadFilePath, map[string]string{"Content-Type": "video/MP2T"})
	if writeChan == nil {
		t.Fatal("Error creating chunked channel")
	}

	writeChan <- []byte("streaming-data")
	close(writeChan)

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("Expected a chunked transfer error, got nil")
		}
		if !errors.Is(err, uploaders.ErrChunkedTransfer) {
			t.Errorf("Expected ErrChunkedTransfer, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timed out waiting for chunked transfer error to be observed")
	}
}

// TestUploadChunkedTransferConnectionBrokenObservable covers the
// connection-dropped variant of the same requirement.
func TestUploadChunkedTransferConnectionBrokenObservable(t *testing.T) {
	uploadFilePath := "test/chunked-broken.ts"
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		hj, ok := rw.(http.Hijacker)
		if !ok {
			t.Error("httptest server does not support hijacking")
			return
		}
		conn, _, errHijack := hj.Hijack()
		if errHijack != nil {
			t.Error("Error hijacking: ", errHijack)
			return
		}
		// Drop the TCP connection immediately, before the client finishes
		// streaming. The in-flight request then fails with a transport error.
		_ = conn.Close()
	}))
	defer server.Close()

	up := newTestUploader(t, server, 2)

	writeChan, errChan := up.UploadChunkedTransfer(uploadFilePath, nil)
	writeChan <- []byte("partial")
	close(writeChan)

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("Expected an error when the connection breaks, got nil")
		}
		if !errors.Is(err, uploaders.ErrChunkedTransfer) {
			t.Errorf("Expected ErrChunkedTransfer, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timed out waiting for the broken-transfer error")
	}
}
