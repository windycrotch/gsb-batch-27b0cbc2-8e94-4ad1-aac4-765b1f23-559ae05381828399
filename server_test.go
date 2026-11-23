package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

func newTestTrack(t *testing.T, kind, codec string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: codec}, kind, "track-"+kind)
	if err != nil {
		t.Fatalf("create %s track: %v", kind, err)
	}
	return track
}

func buildTestServer(t *testing.T) (*Server, *MediaSource, *httptest.Server) {
	t.Helper()

	videoSource := newTestMediaSource(t, "video", nil, 3)
	media := map[string]*MediaSource{"video": videoSource}

	srv := NewServer(ConfigStruct{AllowCrossOrigin: true}, nil, media)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(func() {
		httpSrv.Close()
		srv.Shutdown()
	})
	return srv, videoSource, httpSrv
}

// makeOfferPC builds a browser-side PeerConnection that offers to receive
// video, and returns its marshalled offer SDP.
func makeOfferPC(t *testing.T) (*webrtc.PeerConnection, []byte) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("offer pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gatherComplete

	body, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		t.Fatalf("marshal offer: %v", err)
	}
	return pc, body
}

func doNegotiate(t *testing.T, url string, offerBody []byte, sessionID string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(offerBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(SessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func answerFromBody(t *testing.T, body []byte) webrtc.SessionDescription {
	t.Helper()
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode answer %q: %v", string(body), err)
	}
	return answer
}

// TestConcurrentNegotiation: many viewers negotiate at the same time and each
// receives its own distinct, valid answer. This is the regression test for the
// former single global session description being overwritten.
func TestConcurrentNegotiation(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	const viewers = 12
	var wg sync.WaitGroup
	errCh := make(chan error, viewers)
	answers := make([]webrtc.SessionDescription, viewers)
	answerMu := sync.Mutex{}

	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			offerPC, offerBody := makeOfferPC(t)
			resp, body := doNegotiate(t, httpSrv.URL, offerBody, fmt.Sprintf("viewer-%d", i))
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("viewer %d: status %d body %s", i, resp.StatusCode, body)
				return
			}
			answer := answerFromBody(t, body)
			if answer.Type != webrtc.SDPTypeAnswer {
				errCh <- fmt.Errorf("viewer %d: unexpected SDP type %q", i, answer.Type)
				return
			}
			if err := offerPC.SetRemoteDescription(answer); err != nil {
				errCh <- fmt.Errorf("viewer %d: answer rejected: %w", i, err)
				return
			}
			answerMu.Lock()
			answers[i] = answer
			answerMu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if got := srv.sessions.len(); got != viewers {
		t.Fatalf("live sessions = %d, want %d", got, viewers)
	}

	// Every answer must be unique: a shared description would make several of
	// them identical or mismatched.
	seen := make(map[string]int)
	for i, answer := range answers {
		seen[answer.SDP]++
		if answer.SDP == "" {
			t.Fatalf("viewer %d got empty answer SDP", i)
		}
	}
	if len(seen) != viewers {
		t.Fatalf("expected %d distinct answers, got %d distinct", viewers, len(seen))
	}
}

// TestRenegotiationClosesOldConnection: when the same viewer negotiates again,
// the newest request wins and the previous PeerConnection is closed orderly,
// while other viewers' sessions stay untouched.
func TestRenegotiationClosesOldConnection(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	_, body1 := makeOfferPC(t)
	resp1, answerBody1 := doNegotiate(t, httpSrv.URL, body1, "repeat-viewer")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first negotiation status %d: %s", resp1.StatusCode, answerBody1)
	}
	first, ok := srv.sessions.get("repeat-viewer")
	if !ok {
		t.Fatal("first session not registered")
	}

	// An unrelated viewer that must remain unaffected by the re-negotiation.
	_, otherBody := makeOfferPC(t)
	respOther, _ := doNegotiate(t, httpSrv.URL, otherBody, "other-viewer")
	if respOther.StatusCode != http.StatusOK {
		t.Fatalf("other negotiation status %d", respOther.StatusCode)
	}
	other, ok := srv.sessions.get("other-viewer")
	if !ok {
		t.Fatal("other session not registered")
	}

	_, body2 := makeOfferPC(t)
	resp2, answerBody2 := doNegotiate(t, httpSrv.URL, body2, "repeat-viewer")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second negotiation status %d: %s", resp2.StatusCode, answerBody2)
	}

	second, ok := srv.sessions.get("repeat-viewer")
	if !ok {
		t.Fatal("second session not registered")
	}
	if second == first {
		t.Fatal("re-negotiation did not replace the session")
	}

	waitFor(t, time.Second, first.isClosed, "old PeerConnection closed")
	if other.isClosed() {
		t.Fatal("unrelated viewer's connection was closed by mistake")
	}
	if got := srv.sessions.len(); got != 2 {
		t.Fatalf("live sessions = %d, want 2", got)
	}

	// Closing the old PC twice must be a no-op (sync.Once), never an error.
	first.close()
}

// TestNegotiationFailureIsolation: a malformed offer fails its own request and
// cleans up after itself, while concurrent valid requests keep succeeding and
// existing sessions remain connected.
func TestNegotiationFailureIsolation(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	// Establish a healthy session first.
	_, goodBody := makeOfferPC(t)
	resp, answerBody := doNegotiate(t, httpSrv.URL, goodBody, "healthy-viewer")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup status %d: %s", resp.StatusCode, answerBody)
	}
	healthy, _ := srv.sessions.get("healthy-viewer")

	const badRequests = 5
	var wg sync.WaitGroup
	for i := 0; i < badRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bad := []byte(fmt.Sprintf(`{"type":"offer","sdp":"this is not valid sdp %d"}`, i))
			badResp, _ := doNegotiate(t, httpSrv.URL, bad, fmt.Sprintf("bad-%d", i))
			if badResp.StatusCode == http.StatusOK {
				t.Errorf("bad request %d unexpectedly succeeded", i)
			}
		}(i)
	}

	// A valid request racing the failing ones must still succeed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, racingBody := makeOfferPC(t)
		racingResp, racingAnswer := doNegotiate(t, httpSrv.URL, racingBody, "racing-viewer")
		if racingResp.StatusCode != http.StatusOK {
			t.Errorf("racing valid request failed with %d: %s", racingResp.StatusCode, racingAnswer)
		}
	}()
	wg.Wait()

	if healthy.isClosed() {
		t.Fatal("healthy session was torn down by failing requests")
	}
	if got := srv.sessions.len(); got != 2 {
		t.Fatalf("live sessions = %d, want 2 (bad requests must not leave sessions)", got)
	}

	// Totally malformed JSON yields a 400 rather than a panic/crash.
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL, bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	plainResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("malformed request: %v", err)
	}
	defer plainResp.Body.Close()
	if plainResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", plainResp.StatusCode)
	}
}

// TestPCFactoryFailureDoesNotKillServer: when PeerConnection construction
// fails, the request gets a clear 503 and later requests still work.
func TestPCFactoryFailureDoesNotKillServer(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	factoryErr := errors.New("pc construction injected failure")
	calls := 0
	srv.newPC = func(iceServers []string) (*webrtc.PeerConnection, error) {
		calls++
		if calls == 1 {
			return nil, factoryErr
		}
		return defaultPCFactory(iceServers)
	}

	_, body := makeOfferPC(t)
	resp, respBody := doNegotiate(t, httpSrv.URL, body, "unlucky")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", resp.StatusCode, respBody)
	}
	if srv.sessions.len() != 0 {
		t.Fatal("failed construction must not register a session")
	}

	_, body2 := makeOfferPC(t)
	resp2, answer2 := doNegotiate(t, httpSrv.URL, body2, "lucky")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", resp2.StatusCode, answer2)
	}
}

// TestSessionCloseReleasesConnection: a session reaching Failed/Closed state
// unregisters itself and closes exactly its own PeerConnection.
func TestSessionCloseReleasesConnection(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	_, body := makeOfferPC(t)
	resp, answerBody := doNegotiate(t, httpSrv.URL, body, "closing-viewer")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, answerBody)
	}
	session, ok := srv.sessions.get("closing-viewer")
	if !ok {
		t.Fatal("session not registered")
	}

	session.close()
	waitFor(t, time.Second, func() bool { return session.isClosed() }, "session closed")

	// The closed PeerConnection reports the closed state without panicking.
	if pcState := session.pc.ConnectionState(); pcState != webrtc.PeerConnectionStateClosed {
		t.Fatalf("pc state = %s, want closed", pcState)
	}

	// Explicit manager removal releases the registry entry.
	srv.sessions.remove(session)
	if _, found := srv.sessions.get("closing-viewer"); found {
		t.Fatal("session still registered after removal")
	}

	// A brand new negotiation still works after the cleanup.
	_, body2 := makeOfferPC(t)
	resp2, _ := doNegotiate(t, httpSrv.URL, body2, "fresh-viewer")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("post-close negotiation status = %d", resp2.StatusCode)
	}
}

// TestStateChangeCallbackReleasesSession: when the PeerConnection reaches the
// closed state, the registered OnConnectionStateChange callback unregisters
// the session and releases it.
func TestStateChangeCallbackReleasesSession(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	_, body := makeOfferPC(t)
	resp, answerBody := doNegotiate(t, httpSrv.URL, body, "callback-viewer")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, answerBody)
	}
	session, ok := srv.sessions.get("callback-viewer")
	if !ok {
		t.Fatal("session not registered")
	}

	// Closing the PC fires OnConnectionStateChange(Closed), exactly as a real
	// network failure would fire Failed/Disconnected.
	if err := session.pc.Close(); err != nil {
		t.Fatalf("close pc: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		_, found := srv.sessions.get("callback-viewer")
		return !found
	}, "callback unregistered the failed session")
}

// TestStaleCallbackCannotCloseNewerSession: after re-negotiation, invoking the
// old session's cleanup path must not remove or close the replacement.
func TestStaleCallbackCannotCloseNewerSession(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	_, body1 := makeOfferPC(t)
	if resp, _ := doNegotiate(t, httpSrv.URL, body1, "same-id"); resp.StatusCode != http.StatusOK {
		t.Fatal("first negotiation failed")
	}
	old, _ := srv.sessions.get("same-id")

	_, body2 := makeOfferPC(t)
	if resp, _ := doNegotiate(t, httpSrv.URL, body2, "same-id"); resp.StatusCode != http.StatusOK {
		t.Fatal("second negotiation failed")
	}
	newSession, _ := srv.sessions.get("same-id")

	// Simulate a delayed state callback firing on the old, already-closed PC.
	srv.sessions.remove(old)
	old.close()

	current, ok := srv.sessions.get("same-id")
	if !ok || current != newSession {
		t.Fatal("identity-checked removal deleted the newer session")
	}
	if newSession.isClosed() {
		t.Fatal("newer session was closed by a stale callback")
	}
}

// TestStatusEndpointAndMediaHeaders: viewers can observe media state via the
// status endpoint and the response headers, including the stopped state.
func TestStatusEndpointAndMediaHeaders(t *testing.T) {
	_, videoSource, httpSrv := buildTestServer(t)

	// Park the video media path.
	bindErr := errors.New("port busy")
	videoSource.dialUDP, _ = scriptedDialer(nil, bindErr, nil)
	go videoSource.Run()
	t.Cleanup(videoSource.Stop)
	waitFor(t, time.Second, func() bool {
		return videoSource.Snapshot().State == MediaStateStopped
	}, "media stopped")

	statusResp, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", statusResp.StatusCode)
	}
	var report statusReport
	if err := json.NewDecoder(statusResp.Body).Decode(&report); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if report.Media["video"].State != MediaStateStopped {
		t.Fatalf("reported video state = %s, want stopped", report.Media["video"].State)
	}

	// Negotiation still succeeds while the media path is stopped, and the
	// response carries an explicit media-state header.
	_, body := makeOfferPC(t)
	resp, answerBody := doNegotiate(t, httpSrv.URL, body, "header-viewer")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("negotiation during media outage status = %d: %s", resp.StatusCode, answerBody)
	}
	if got := resp.Header.Get("X-RTP2WEBRTC-Media-Video"); got != "stopped" {
		t.Fatalf("media header = %q, want stopped", got)
	}
	if sid := resp.Header.Get(SessionIDHeader); sid != "header-viewer" {
		t.Fatalf("session id header = %q", sid)
	}
}

// TestSessionIDFallbackAndQueryParam: viewers without an explicit id receive a
// generated one; the query parameter is accepted as an identifier too.
func TestSessionIDFallbackAndQueryParam(t *testing.T) {
	srv, _, httpSrv := buildTestServer(t)

	_, body := makeOfferPC(t)
	resp, _ := doNegotiate(t, httpSrv.URL, body, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous negotiation failed: %d", resp.StatusCode)
	}
	if resp.Header.Get(SessionIDHeader) == "" {
		t.Fatal("anonymous viewer did not receive a generated session id")
	}

	_, body2 := makeOfferPC(t)
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"?sessionId=query-viewer", bytes.NewReader(body2))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("query request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("query-param negotiation failed: %d", resp2.StatusCode)
	}
	if _, ok := srv.sessions.get("query-viewer"); !ok {
		t.Fatal("query parameter session id was not registered")
	}
}

// TestCORSOptionsRequest: preflight requests are answered without building a
// PeerConnection.
func TestCORSOptionsRequest(t *testing.T) {
	_, _, httpSrv := buildTestServer(t)
	req, _ := http.NewRequest(http.MethodOptions, httpSrv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("options status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing CORS header")
	}
}
