/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
	"sigs.k8s.io/controller-runtime/pkg/log"

	enginev1 "github.com/nagare-media/engine/api/v1alpha1"
	engineio "github.com/nagare-media/engine/internal/functions/io"
)

func TestMain(m *testing.M) {
	log.SetLogger(logr.Discard())
	os.Exit(m.Run())
}

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mtx sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.buf.String()
}

func newTestPort(pt engineio.PortType, d enginev1.MediaDirection, rawurl string) *port {
	u, err := url.Parse(rawurl)
	if err != nil {
		panic(err)
	}
	pr, pw := engineio.Pipe()
	return &port{
		pt:           pt,
		d:            d,
		name:         "test",
		url:          u,
		client:       &fasthttp.Client{},
		pr:           pr,
		pw:           pw,
		closeTimeout: CloseTimeout,
		maxTries:     MaxRetries,
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func (p *port) timerArmed() bool {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	return p.closeTimer != nil
}

// startTestServer starts a fiber app on a random local port and returns its
// base URL.
func startTestServer(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	return "http://" + ln.Addr().String()
}

func startPort(t *testing.T, p *port) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		errCh <- p.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = p.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("port Start did not terminate")
		}
	})
	return cancel, errCh
}

// TestInputPushPortReceivesData tests the normal receive path of an input
// (push) port: POSTed data is forwarded to the connected stream.
func TestInputPushPortReceivesData(t *testing.T) {
	p := newTestPort(engineio.InputPortType, enginev1.PushMediaDirection, "http://example.com/in")
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	startPort(t, p)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/in", p.handlePost)
	base := startTestServer(t, app)

	resp, err := http.Post(base+"/in", "application/octet-stream", bytes.NewReader([]byte("hello")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}

	waitFor(t, 5*time.Second, func() bool { return buf.String() == "hello" }, "connection received POSTed data")
}

// TestInputPushPortCloseTimer tests that the close timer of an input (push)
// port is aborted when new data arrives before the timeout, and that the port
// is closed (and rejects further requests) once the timeout expires.
func TestInputPushPortCloseTimer(t *testing.T) {
	p := newTestPort(engineio.InputPortType, enginev1.PushMediaDirection, "http://example.com/in")
	p.closeTimeout = 300 * time.Millisecond
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	startPort(t, p)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/in", p.handlePost)
	base := startTestServer(t, app)

	post := func() int {
		resp, err := http.Post(base+"/in", "application/octet-stream", bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// first POST starts the close timer
	if sc := post(); sc != fiber.StatusCreated {
		t.Fatalf("unexpected status code: %d", sc)
	}
	if !p.timerArmed() {
		t.Fatal("close timer not started after receiving data")
	}

	// POSTs before the timeout abort the scheduled close and keep the port
	// alive
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		if p.isClosed() {
			t.Fatal("port closed although data kept arriving")
		}
		if sc := post(); sc != fiber.StatusCreated {
			t.Fatalf("unexpected status code in iteration %d: %d", i, sc)
		}
	}

	// without new data the port is closed after the timeout
	waitFor(t, 5*time.Second, p.isClosed, "port closed after inactivity timeout")

	// a closed port rejects new requests and is not revived
	if sc := post(); sc != fiber.StatusGone {
		t.Fatalf("expected status 410 after close, got: %d", sc)
	}
}

// TestInputPushPortConcurrentWrites tests that concurrent requests to an
// input (push) port are serialized: each request body must be written to the
// pipe contiguously.
func TestInputPushPortConcurrentWrites(t *testing.T) {
	p := newTestPort(engineio.InputPortType, enginev1.PushMediaDirection, "http://example.com/in")
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	startPort(t, p)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/in", p.handlePost)
	base := startTestServer(t, app)

	const n = 8
	payloads := make([]string, n)
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		payloads[i] = fmt.Sprintf("payload-%02d:%s", i, strings.Repeat(string(rune('a'+i)), 4096))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(base+"/in", "application/octet-stream", strings.NewReader(payloads[i]))
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
				return
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, sc := range statuses {
		if sc != fiber.StatusCreated {
			t.Errorf("request %d: unexpected status code: %d", i, sc)
		}
	}

	waitFor(t, 5*time.Second, func() bool {
		return len(buf.String()) == n*len(payloads[0])
	}, "all request bodies written to the pipe")

	got := buf.String()
	for i, payload := range payloads {
		if !strings.Contains(got, payload) {
			t.Errorf("payload %d not found contiguously in stream", i)
		}
	}
}

// TestInputPullPortFetchesWithRetries tests that an input (pull) port retries
// failed requests and eventually delivers the streamed data.
func TestInputPullPortFetchesWithRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("stream-data"))
	}))
	defer srv.Close()

	p := newTestPort(engineio.InputPortType, enginev1.PullMediaDirection, srv.URL)
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	startPort(t, p)

	waitFor(t, 10*time.Second, func() bool { return buf.String() == "stream-data" }, "connection received pulled data")

	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	if p.isClosed() {
		t.Fatal("port must stay usable while retrying")
	}
}

// TestInputPullPortRetryCancelsScheduledClose tests that a successful retry
// aborts a close that was scheduled after the previous attempt received data.
func TestInputPullPortRetryCancelsScheduledClose(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// stream some data, then drop the connection mid-stream
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("part1"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("part2"))
	}))
	defer srv.Close()

	p := newTestPort(engineio.InputPortType, enginev1.PullMediaDirection, srv.URL)
	p.closeTimeout = 30 * time.Second // must not fire during the test
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	startPort(t, p)

	waitFor(t, 10*time.Second, func() bool { return buf.String() == "part1part2" }, "connection received data of both attempts")

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
	if p.isClosed() {
		t.Fatal("successful retry must have aborted the scheduled close")
	}
}

// TestInputPullPortRetryExhausted tests that an input (pull) port fails with
// a diagnosable error once the bounded retries are exhausted.
func TestInputPullPortRetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := newTestPort(engineio.InputPortType, enginev1.PullMediaDirection, srv.URL)
	p.maxTries = 3
	defer p.Close()

	err := p.getRequest(context.Background())
	if err == nil {
		t.Fatal("expected error after retries were exhausted")
	}
	if !strings.Contains(err.Error(), "unexpected HTTP status code") {
		t.Fatalf("error is not diagnosable: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	if p.isClosed() {
		t.Fatal("exhausted retries must not close the port")
	}
}

// TestInputPullPortRetryAfterCloseFails tests that a retry does not revive a
// port whose close already took effect; instead a clear error is returned.
func TestInputPullPortRetryAfterCloseFails(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := newTestPort(engineio.InputPortType, enginev1.PullMediaDirection, srv.URL)
	p.maxTries = 10

	// close the port while the retry backoff is sleeping
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
	}()

	err := p.getRequest(context.Background())
	if !errors.Is(err, ErrPortClosed) {
		t.Fatalf("expected ErrPortClosed, got: %v", err)
	}
	if got := attempts.Load(); got > 2 {
		t.Fatalf("retries continued after port was closed: %d attempts", got)
	}
}

// TestOutputPushPortPostsWithRetry tests that an output (push) port retries
// connecting to a restarted downstream and eventually delivers the data.
func TestOutputPushPortPostsWithRetry(t *testing.T) {
	// reserve an address, then release it: initially nothing listens there
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	received := make(chan string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- string(body)
			w.WriteHeader(http.StatusOK)
		}),
	}

	p := newTestPort(engineio.OutputPortType, enginev1.PushMediaDirection, "http://"+addr+"/up")
	p.maxTries = 20
	defer p.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- p.postRequest(context.Background()) }()

	// let the first attempt(s) fail while nothing is listening
	time.Sleep(300 * time.Millisecond)

	// "restart" the downstream
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-listen: %v", err)
	}
	go func() { _ = srv.Serve(ln2) }()
	defer srv.Close()

	// this data is consumed by the request body of a retry
	if _, err := p.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// closing the port ends the request body
	_ = p.Close()

	select {
	case got := <-received:
		if got != "hello" {
			t.Fatalf("downstream received unexpected body: %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downstream did not receive data")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("postRequest: %v", err)
	}
}

// TestOutputPushPortRetryExhausted tests that an output (push) port fails
// with a diagnosable error once the bounded retries are exhausted.
func TestOutputPushPortRetryExhausted(t *testing.T) {
	// nothing listens on this address
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := newTestPort(engineio.OutputPortType, enginev1.PushMediaDirection, "http://"+addr+"/up")
	p.maxTries = 3
	defer p.Close()

	err = p.postRequest(context.Background())
	if err == nil {
		t.Fatal("expected error after retries were exhausted")
	}
	if p.isClosed() {
		t.Fatal("exhausted retries must not close the port")
	}
}

// TestOutputPushPortRetryAfterCloseFails tests that a retry does not revive a
// closed output (push) port; instead a clear error is returned.
func TestOutputPushPortRetryAfterCloseFails(t *testing.T) {
	// nothing listens on this address
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := newTestPort(engineio.OutputPortType, enginev1.PushMediaDirection, "http://"+addr+"/up")
	p.maxTries = 10

	// close the port while the retry backoff is sleeping
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
	}()

	err = p.postRequest(context.Background())
	if !errors.Is(err, ErrPortClosed) {
		t.Fatalf("expected ErrPortClosed, got: %v", err)
	}
}

// TestOutputPullPortStreamLifecycle tests an output (pull) port: it serves
// stream data, starts the close timer once a client leaves, aborts the close
// when a new request arrives in time, and finally closes (rejecting further
// requests) after the inactivity timeout expires.
func TestOutputPullPortStreamLifecycle(t *testing.T) {
	// 8 KiB chunk; larger than the server write buffer so it is flushed
	// immediately. The 16 byte period keeps chunk boundaries aligned.
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 512)

	p := newTestPort(engineio.OutputPortType, enginev1.PullMediaDirection, "http://example.com/out")
	p.closeTimeout = time.Second

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/out", p.handleGet)
	base := startTestServer(t, app)

	client := &http.Client{Timeout: 15 * time.Second}

	var writers sync.WaitGroup
	stopWrites := make(chan struct{})
	defer func() {
		close(stopWrites)
		_ = p.Close()
		writers.Wait()
	}()
	writeChunks := func() {
		defer writers.Done()
		for {
			select {
			case <-stopWrites:
				return
			default:
			}
			if p.isClosed() {
				return
			}
			if _, err := p.Write(chunk); err != nil {
				return
			}
		}
	}

	readChunk := func(t *testing.T, r io.Reader) {
		t.Helper()
		got := make([]byte, len(chunk))
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		if !bytes.Equal(got, chunk) {
			t.Fatal("received unexpected chunk data")
		}
	}

	// --- normal streaming ---
	writers.Add(1)
	go writeChunks()
	resp1, err := client.Get(base + "/out")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("unexpected status code: %d", resp1.StatusCode)
	}
	readChunk(t, resp1.Body)
	// client leaves (e.g. downstream restart)
	resp1.Body.Close()

	// --- close timer starts once the response stream terminates ---
	waitFor(t, 10*time.Second, p.timerArmed, "close timer started after client left")

	// --- a new request before the timeout aborts the scheduled close ---
	resp2, err := client.Get(base + "/out")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp2.StatusCode != fiber.StatusOK {
		t.Fatalf("request within close timeout was rejected: %d", resp2.StatusCode)
	}
	if p.timerArmed() {
		t.Fatal("new request did not abort the scheduled close")
	}
	readChunk(t, resp2.Body)
	resp2.Body.Close()

	// --- without a new request the port is closed after the timeout ---
	waitFor(t, 10*time.Second, p.timerArmed, "close timer restarted after client left")
	waitFor(t, 10*time.Second, p.isClosed, "port closed after inactivity timeout")

	// --- a closed port rejects new requests and is not revived ---
	resp3, err := client.Get(base + "/out")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != fiber.StatusGone {
		t.Fatalf("expected status 410 after close, got: %d", resp3.StatusCode)
	}
}

// TestPortCloseIsIdempotentAndReleasesResources tests that closing a port
// stops the close timer, releases resources and is safe to do repeatedly.
func TestPortCloseIsIdempotentAndReleasesResources(t *testing.T) {
	p := newTestPort(engineio.InputPortType, enginev1.PushMediaDirection, "http://example.com/in")
	p.closeTimeout = time.Hour
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))
	_, errCh := startPort(t, p)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/in", p.handlePost)
	base := startTestServer(t, app)

	resp, err := http.Post(base+"/in", "application/octet-stream", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}
	if !p.timerArmed() {
		t.Fatal("close timer not started after receiving data")
	}

	// closing stops the timer and releases the pipe
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !p.isClosed() {
		t.Fatal("port not marked closed")
	}
	if p.timerArmed() {
		t.Fatal("close timer not stopped")
	}

	// repeated closes must neither panic nor error
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}

	// the port's Start loop terminates
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("port Start did not terminate after Close")
	}

	// requests after close are rejected
	resp, err = http.Post(base+"/in", "application/octet-stream", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusGone {
		t.Fatalf("expected status 410 after close, got: %d", resp.StatusCode)
	}
}

// TestStartClosesPortOnContextCancel tests that all port resources are
// released when the function terminates.
func TestStartClosesPortOnContextCancel(t *testing.T) {
	p := newTestPort(engineio.InputPortType, enginev1.PushMediaDirection, "http://example.com/in")
	buf := &syncBuffer{}
	p.Connect(engineio.NewConnection(buf))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.Start(ctx) }()

	cancel()
	waitFor(t, 10*time.Second, p.isClosed, "port closed on context cancel")

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("port Start did not terminate on context cancel")
	}
}
