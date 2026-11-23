package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/pion/webrtc/v3"
)

// SessionIDHeader lets a viewer identify itself across re-negotiations. The
// same identifier is accepted through the "sessionId" query parameter.
const SessionIDHeader = "X-RTP2WEBRTC-Session-Id"

// pcFactory builds PeerConnections; it is a field so tests can inject failures.
type pcFactory func(iceServers []string) (*webrtc.PeerConnection, error)

func defaultPCFactory(iceServers []string) (*webrtc.PeerConnection, error) {
	urls := make([]string, 0, len(iceServers))
	urls = append(urls, iceServers...)
	return webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: urls}},
	})
}

// Server owns the HTTP negotiation endpoint, the media sources and the
// viewer session registry. It contains no mutable per-request state, so any
// number of requests can be served concurrently without interfering.
type Server struct {
	config     ConfigStruct
	iceServers []string
	media      map[string]*MediaSource
	sessions   *SessionManager
	newPC      pcFactory
}

func NewServer(cfg ConfigStruct, iceServers []string, media map[string]*MediaSource) *Server {
	return &Server{
		config:     cfg,
		iceServers: iceServers,
		media:      media,
		sessions:   NewSessionManager(),
		newPC:      defaultPCFactory,
	}
}

// statusReport is the JSON body returned by the status endpoint and embedded
// in negotiation error responses.
type statusReport struct {
	Sessions int                      `json:"sessions"`
	Media    map[string]MediaSnapshot `json:"media"`
}

func (s *Server) buildStatus() statusReport {
	report := statusReport{
		Sessions: s.sessions.len(),
		Media:    make(map[string]MediaSnapshot, len(s.media)),
	}
	for kind, source := range s.media {
		report.Media[kind] = source.Snapshot()
	}
	return report
}

// ServeHTTP handles one negotiation request. Every request decodes its own
// session description into a local variable: a shared global description can
// no longer be overwritten by concurrent requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Last-resort guard: an unexpected panic must answer this single request
	// with a 500 instead of tearing down the whole process and every viewer.
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("panic while serving negotiation request: %v", recovered)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()

	if s.config.AllowCrossOrigin {
		header := w.Header()
		header.Add("Access-Control-Allow-Origin", "*")
		header.Add("Access-Control-Allow-Methods", "DELETE, POST, GET, OPTIONS")
		header.Add("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		header.Add("Access-Control-Allow-Private-Network", "Content-Type, Authorization, X-Requested-With")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// A GET on the negotiation endpoint exposes the observable state of the
	// media paths and the number of live sessions.
	if r.Method == http.MethodGet {
		s.writeStatus(w, http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		log.Printf("rejected %s request from %s: invalid HTTP method", r.Method, r.RemoteAddr)
		http.Error(w, "Invalid HTTP method", http.StatusBadRequest)
		return
	}

	sessionID := r.Header.Get(SessionIDHeader)
	if sessionID == "" {
		sessionID = r.URL.Query().Get("sessionId")
	}
	if sessionID == "" {
		sessionID = newSessionID()
	}

	answer, err := s.negotiate(r, sessionID)
	if err != nil {
		var negotiationErr *negotiationError
		if errors.As(err, &negotiationErr) {
			log.Printf("negotiation failed for viewer %s: %v", sessionID, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(negotiationErr.status)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error":  negotiationErr.Error(),
				"status": s.buildStatus(),
			})
			return
		}
		log.Printf("negotiation failed for viewer %s: %v", sessionID, err)
		http.Error(w, "Negotiation failed", http.StatusInternalServerError)
		return
	}

	s.writeMediaHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(SessionIDHeader, sessionID)
	if _, err := w.Write(answer); err != nil {
		log.Printf("writing answer for viewer %s failed: %v", sessionID, err)
	}
}

// negotiationError carries an HTTP status code for an expected negotiation
// failure so the handler can answer precisely without panicking.
type negotiationError struct {
	status int
	err    error
}

func (e *negotiationError) Error() string {
	if e.err == nil {
		return http.StatusText(e.status)
	}
	return e.err.Error()
}

func (e *negotiationError) Unwrap() error { return e.err }

// negotiate runs the full WebRTC answer flow for one viewer and returns the
// marshalled answer. On failure the partially built session is cleaned up and
// no other session is affected.
func (s *Server) negotiate(r *http.Request, sessionID string) ([]byte, error) {
	// Local to this request: concurrent requests each have their own offer.
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		return nil, &negotiationError{status: http.StatusBadRequest, err: errBadSessionDescription}
	}

	pc, err := s.newPC(s.iceServers)
	if err != nil {
		return nil, &negotiationError{status: http.StatusServiceUnavailable, err: err}
	}

	for _, kind := range []string{"video", "audio"} {
		source, ok := s.media[kind]
		if !ok {
			continue
		}
		if _, err := pc.AddTrack(source.Track()); err != nil {
			_ = pc.Close()
			return nil, &negotiationError{status: http.StatusInternalServerError, err: err}
		}
	}

	session := s.sessions.add(sessionID, pc)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("viewer %s connection state: %s", sessionID, state)
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			// Identity-checked removal guarantees a stale callback can never
			// close a newer session belonging to the same or another viewer.
			s.sessions.remove(session)
			session.close()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		s.sessions.remove(session)
		session.close()
		return nil, &negotiationError{status: http.StatusBadRequest, err: err}
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.sessions.remove(session)
		session.close()
		return nil, &negotiationError{status: http.StatusBadGateway, err: err}
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.sessions.remove(session)
		session.close()
		return nil, &negotiationError{status: http.StatusBadGateway, err: err}
	}
	<-gatherComplete

	marshalled, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		s.sessions.remove(session)
		session.close()
		return nil, &negotiationError{status: http.StatusInternalServerError, err: err}
	}
	return marshalled, nil
}

var errBadSessionDescription = errors.New("bad session description")

func (s *Server) writeStatus(w http.ResponseWriter, status int) {
	s.writeMediaHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(s.buildStatus()); err != nil {
		log.Printf("writing status report failed: %v", err)
	}
}

// writeMediaHeaders tells each new viewer, up front, which media paths are
// currently healthy.
func (s *Server) writeMediaHeaders(w http.ResponseWriter) {
	for kind, source := range s.media {
		w.Header().Set("X-RTP2WEBRTC-Media-"+canonicalHeader(kind), source.Snapshot().State.String())
	}
}

func canonicalHeader(kind string) string {
	if kind == "" {
		return ""
	}
	out := make([]byte, 0, len(kind))
	upper := true
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		upper = false
		out = append(out, c)
	}
	return string(out)
}

// initWebrtcTrack creates the static local RTP track for one media kind.
func initWebrtcTrack(mediaType string, codec string, trackName string) (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: codec},
		mediaType,
		trackName,
	)
}

// Shutdown orderly releases every viewer session. Media sources are stopped by
// the caller because their lifecycle is started alongside main().
func (s *Server) Shutdown() {
	s.sessions.closeAll()
}
