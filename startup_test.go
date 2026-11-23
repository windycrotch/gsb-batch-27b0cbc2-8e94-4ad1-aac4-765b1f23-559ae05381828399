// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

// stubFactories swaps the package-level endpoint factories for fakes and
// restores them automatically when the test ends.
func stubFactories(t *testing.T, dial func(h *Handler, host, p, key string) (Endpoint, error),
	flv func(baseDir, filename string) (Endpoint, error)) {
	t.Helper()

	origDial := dialRemoteEndpoint
	origFlv := createFlvFileEndpoint
	dialRemoteEndpoint = dial
	createFlvFileEndpoint = flv
	t.Cleanup(func() {
		dialRemoteEndpoint = origDial
		createFlvFileEndpoint = origFlv
	})
}

func profileHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandler()
	h.ConnMsg = &rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{},
	}
	h.PubMsg = &rtmpmsg.NetStreamPublish{PublishingName: "live/key"}
	u, err := url.Parse("rtmp://example.com/profile/test")
	if err != nil {
		t.Fatal(err)
	}
	h.Url = u
	return h
}

func writeProfile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Profile mode builds endpoints one by one. If a later remote fails to
// connect, every endpoint already established must be closed - no dangling
// connections, no half-written files.
func TestProfileStartupFailureClosesEstablishedEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "test", `{
		"remotes": [
			{"host": "a.example", "path": "live", "key": "k1"},
			{"host": "b.example", "path": "live", "key": "k2"},
			{"host": "c.example", "path": "live", "key": "k3"}
		],
		"recording": true
	}`)

	established := []*fakeEndpoint{}
	var dialCnt int64
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) {
			n := atomic.AddInt64(&dialCnt, 1)
			if n == 3 {
				return nil, errors.New("simulated dial failure on third remote")
			}
			ep := &fakeEndpoint{name: "remote-" + host}
			established = append(established, ep)
			return ep, nil
		},
		func(baseDir, filename string) (Endpoint, error) {
			ep := &fakeEndpoint{name: "flv"}
			established = append(established, ep)
			return ep, nil
		},
	)

	h := profileHandler(t)
	err := handleProfile(h, dir, "test")
	if err == nil {
		t.Fatal("expected startup error, got nil")
	}

	// Two remotes succeeded before the third dial failed; recording was
	// never reached. All established endpoints must be closed exactly once.
	if len(established) != 2 {
		t.Fatalf("expected 2 established endpoints, got %d", len(established))
	}
	for _, ep := range established {
		if _, _, _, closes := ep.counts(); closes != 1 {
			t.Fatalf("endpoint %s closed %d times during cleanup, want 1", ep.name, closes)
		}
	}

	h.mu.RLock()
	remaining := len(h.endpoints)
	closed := h.closed
	h.mu.RUnlock()
	if remaining != 0 || !closed {
		t.Fatalf("handler must retain no endpoints and be marked closed, got %d endpoints, closed=%v", remaining, closed)
	}
}

// When recording file creation fails, the remotes already connected must be
// closed during startup cleanup.
func TestProfileRecordingFailureClosesRemotes(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "rec", `{
		"remotes": [{"host": "a.example", "path": "live", "key": "k"}],
		"recording": true
	}`)

	remote := &fakeEndpoint{name: "remote"}
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) { return remote, nil },
		func(baseDir, filename string) (Endpoint, error) {
			return nil, errors.New("simulated file creation failure")
		},
	)

	h := profileHandler(t)
	if err := handleProfile(h, dir, "rec"); err == nil {
		t.Fatal("expected startup error, got nil")
	}
	if _, _, _, closes := remote.counts(); closes != 1 {
		t.Fatalf("established remote closed %d times after recording failure, want 1", closes)
	}
}

// afterStart webhook failure happens after all endpoints are up; they still
// have to be torn down, and the afterClose hook may run at most once.
func TestProfileAfterStartFailureCleansUpAndHooksOnce(t *testing.T) {
	dir := t.TempDir()

	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // requests now fail with a connection error

	writeProfile(t, dir, "hooked", `{
		"remotes": [{"host": "a.example", "path": "live", "key": "k"}],
		"recording": false,
		"webhooks": {
			"afterStart": [{"method": "GET", "url": "`+deadURL+`", "body_encoding": "string", "body": ""}],
			"afterClose": [{"method": "GET", "url": "`+deadURL+`", "body_encoding": "string", "body": ""}]
		}
	}`)

	remote := &fakeEndpoint{name: "remote"}
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) { return remote, nil },
		func(baseDir, filename string) (Endpoint, error) { return &fakeEndpoint{name: "flv"}, nil },
	)

	h := profileHandler(t)
	if err := handleProfile(h, dir, "hooked"); err == nil {
		t.Fatal("expected afterStart failure, got nil")
	}
	if _, _, _, closes := remote.counts(); closes != 1 {
		t.Fatalf("remote closed %d times, want 1", closes)
	}

	// Startup failure already ran the single Close; a later OnClose must
	// neither re-close the endpoint nor re-run the afterClose hook.
	var hookRuns int64
	h.HooksOnClose = append(h.HooksOnClose, func() { atomic.AddInt64(&hookRuns, 1) })
	h.OnClose()
	h.Close()
	if got := atomic.LoadInt64(&hookRuns); got != 0 {
		t.Fatalf("hook appended after close must not run retroactively; got %d", got)
	}
	if _, _, _, closes := remote.counts(); closes != 1 {
		t.Fatalf("remote re-closed after startup cleanup: %d closes", closes)
	}
}

// Successful startup must leave all endpoints open and run no cleanup.
func TestProfileStartupSuccessKeepsEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "ok", `{
		"remotes": [{"host": "a.example", "path": "live", "key": "k"}],
		"recording": true
	}`)

	remote := &fakeEndpoint{name: "remote"}
	flv := &fakeEndpoint{name: "flv"}
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) { return remote, nil },
		func(baseDir, filename string) (Endpoint, error) { return flv, nil },
	)

	h := profileHandler(t)
	if err := handleProfile(h, dir, "ok"); err != nil {
		t.Fatalf("startup failed: %v", err)
	}
	for _, ep := range []*fakeEndpoint{remote, flv} {
		if _, _, _, closes := ep.counts(); closes != 0 {
			t.Fatalf("endpoint %s must stay open after successful startup, closes=%d", ep.name, closes)
		}
	}

	// A later write failure on one endpoint must not disturb the stream.
	remote.failWrite = true
	if err := h.OnAudio(0, bytes.NewReader(nil)); err != nil {
		t.Fatalf("post-start write failure must be isolated: %v", err)
	}
	if _, _, _, closes := remote.counts(); closes != 1 {
		t.Fatalf("failing remote closed once, got %d", closes)
	}
	if a, _, _, _ := flv.counts(); a != 1 {
		t.Fatalf("recording must keep receiving, audio writes=%d", a)
	}
}

// The full route entry point: on startup failure the client gets the error
// and the stream never enters the "Streaming started" state.
func TestHandleConnStartupFailureReportsErrorAndNoStartedState(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "fail", `{
		"remotes": [
			{"host": "a.example", "path": "live", "key": "k"},
			{"host": "b.example", "path": "live", "key": "k"}
		]
	}`)

	first := &fakeEndpoint{name: "first"}
	var dialCnt int64
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) {
			if atomic.AddInt64(&dialCnt, 1) == 2 {
				return nil, errors.New("simulated second dial failure")
			}
			return first, nil
		},
		func(baseDir, filename string) (Endpoint, error) { return &fakeEndpoint{}, nil },
	)

	h := profileHandler(t)
	// handleConn routes via the URL path and the global profileDir flag,
	// so point both at the profile written in the temp directory.
	u, err := url.Parse("rtmp://example.com/profile/fail")
	if err != nil {
		t.Fatal(err)
	}
	h.Url = u
	origProfileDir := *profileDir
	*profileDir = dir
	t.Cleanup(func() { *profileDir = origProfileDir })

	// Capture log output to prove "Streaming started." is never printed.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origOut := os.Stdout
	os.Stdout = w
	connErr := handleConn(h)
	_ = w.Close()
	os.Stdout = origOut

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	if connErr == nil {
		t.Fatal("handleConn must return the startup error")
	}
	if strings.Contains(buf.String(), "Streaming started") {
		t.Fatalf("stream must not enter started state on failure, logs:\n%s", buf.String())
	}
	if _, _, _, closes := first.counts(); closes != 1 {
		t.Fatalf("established endpoint closed %d times on startup failure, want 1", closes)
	}
}

// Record route with an unwritable output directory: error surfaces and no
// endpoint leaks.
func TestHandleRecordFailure(t *testing.T) {
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) { return &fakeEndpoint{}, nil },
		func(baseDir, filename string) (Endpoint, error) {
			return nil, errors.New("simulated flv create failure")
		},
	)

	h := profileHandler(t)
	if err := handleRecord(h); err == nil {
		t.Fatal("expected record startup failure")
	}
	h.mu.RLock()
	remaining := len(h.endpoints)
	h.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("no endpoint may remain after record failure, got %d", remaining)
	}

	// Direct route: remote established, then file creation fails.
	remote := &fakeEndpoint{name: "remote"}
	stubFactories(t,
		func(h *Handler, host, p, key string) (Endpoint, error) { return remote, nil },
		func(baseDir, filename string) (Endpoint, error) {
			return nil, errors.New("simulated flv create failure")
		},
	)

	h2 := profileHandler(t)
	if err := handleDirect(h2, "a.example", "live"); err == nil {
		t.Fatal("expected direct startup failure")
	}
	if _, _, _, closes := remote.counts(); closes != 1 {
		t.Fatalf("remote must be closed when file creation fails, closes=%d", closes)
	}
}

// Real FlvFile integration: a non-existent output directory makes creation
// fail and the error propagates through the production factory.
func TestRealFlvFileCreationFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := CreateFlvFile(missing, "x"); err == nil {
		t.Fatal("expected file creation error in missing directory")
	}
}
