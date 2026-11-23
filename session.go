package main

import (
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
)

// viewerSession is one viewer's WebRTC connection plus the bookkeeping needed
// to release it deterministically.
type viewerSession struct {
	id        string
	pc        *webrtc.PeerConnection
	createdAt time.Time

	closeOnce sync.Once
	closed    bool
	mu        sync.Mutex
}

// close releases the PeerConnection and its registered callbacks exactly once.
// It never touches any other session.
func (s *viewerSession) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		// Detach the callbacks first so the close below cannot re-enter the
		// state-change handler.
		s.pc.OnConnectionStateChange(nil)
		s.pc.OnICEConnectionStateChange(nil)
		s.pc.OnICECandidate(nil)
		s.pc.OnTrack(nil)
		s.pc.OnDataChannel(nil)

		if err := s.pc.Close(); err != nil {
			// Already-closed connections are not an error worth surfacing.
			log.Printf("closing session %s: %v", s.id, err)
		}
	})
}

func (s *viewerSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// SessionManager keeps track of every viewer session. All access is guarded so
// concurrent negotiations can never corrupt each other's state.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*viewerSession
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*viewerSession)}
}

// add registers a session under id. If the same viewer already has a session
// (re-negotiation), the previous PeerConnection is closed orderly before the
// new one is installed, so the old connection stops holding resources.
func (sm *SessionManager) add(id string, pc *webrtc.PeerConnection) *viewerSession {
	session := &viewerSession{
		id:        id,
		pc:        pc,
		createdAt: time.Now(),
	}

	sm.mu.Lock()
	previous := sm.sessions[id]
	sm.sessions[id] = session
	sm.mu.Unlock()

	if previous != nil {
		log.Printf("viewer %s re-negotiated: closing previous session", id)
		previous.close()
	}
	return session
}

// remove unregisters the session only if the registered instance is still the
// same one. A newer re-negotiation that already replaced it is left untouched,
// so a stale disconnect callback can never close another viewer's connection.
func (sm *SessionManager) remove(session *viewerSession) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if current, ok := sm.sessions[session.id]; ok && current == session {
		delete(sm.sessions, session.id)
	}
}

// get returns the session registered under id, if any.
func (sm *SessionManager) get(id string) (*viewerSession, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	session, ok := sm.sessions[id]
	return session, ok
}

// len returns the number of live sessions.
func (sm *SessionManager) len() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// closeAll orderly closes every registered session (used on shutdown).
func (sm *SessionManager) closeAll() {
	sm.mu.Lock()
	sessions := make([]*viewerSession, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		sessions = append(sessions, session)
	}
	sm.sessions = make(map[string]*viewerSession)
	sm.mu.Unlock()

	for _, session := range sessions {
		session.close()
	}
}

// newSessionID generates an identifier for viewers that do not provide one.
func newSessionID() string {
	return uuid.NewString()
}
