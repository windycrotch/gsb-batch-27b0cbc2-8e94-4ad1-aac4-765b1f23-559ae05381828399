package main

import (
	"sync"
	"testing"

	"github.com/pion/webrtc/v3"
)

func newTestPC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new pc: %v", err)
	}
	return pc
}

// TestSessionManagerAddReplaceAndRemove covers registration, orderly
// replacement on re-negotiation and identity-checked removal.
func TestSessionManagerAddReplaceAndRemove(t *testing.T) {
	sm := NewSessionManager()

	pc1 := newTestPC(t)
	s1 := sm.add("viewer-a", pc1)
	if sm.len() != 1 {
		t.Fatalf("len = %d, want 1", sm.len())
	}

	pc2 := newTestPC(t)
	s2 := sm.add("viewer-b", pc2)
	if sm.len() != 2 {
		t.Fatalf("len = %d, want 2", sm.len())
	}

	// Re-negotiation of viewer-a replaces and closes the old session only.
	pc3 := newTestPC(t)
	s3 := sm.add("viewer-a", pc3)
	if sm.len() != 2 {
		t.Fatalf("len = %d, want 2 after replacement", sm.len())
	}
	if !s1.isClosed() {
		t.Fatal("old session should be closed on replacement")
	}
	if got, ok := sm.get("viewer-a"); !ok || got != s3 {
		t.Fatal("replacement session not registered")
	}
	if s2.isClosed() {
		t.Fatal("unrelated session was closed")
	}

	// Stale removal of the old session must not delete the new one.
	sm.remove(s1)
	if _, ok := sm.get("viewer-a"); !ok {
		t.Fatal("stale removal deleted the current session")
	}

	// Legitimate removal works.
	sm.remove(s3)
	if _, ok := sm.get("viewer-a"); ok {
		t.Fatal("session still present after removal")
	}
}

// TestSessionCloseIsIdempotent: repeated close calls only close once and never
// panic even after the PeerConnection itself has been closed.
func TestSessionCloseIsIdempotent(t *testing.T) {
	sm := NewSessionManager()
	pc := newTestPC(t)
	session := sm.add("viewer-x", pc)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session.close()
		}()
	}
	wg.Wait()

	if !session.isClosed() {
		t.Fatal("session should be closed")
	}
	if state := pc.ConnectionState(); state != webrtc.PeerConnectionStateClosed {
		t.Fatalf("pc state = %s, want closed", state)
	}
}

// TestSessionManagerCloseAll: shutdown releases every session while leaving no
// entries behind.
func TestSessionManagerCloseAll(t *testing.T) {
	sm := NewSessionManager()
	sessions := make([]*viewerSession, 0, 4)
	for i := 0; i < 4; i++ {
		sessions = append(sessions, sm.add(string(rune('a'+i)), newTestPC(t)))
	}

	sm.closeAll()

	if sm.len() != 0 {
		t.Fatalf("len after closeAll = %d, want 0", sm.len())
	}
	for _, session := range sessions {
		if !session.isClosed() {
			t.Fatalf("session %s not closed", session.id)
		}
	}

	// CloseAll on an empty manager must not panic.
	sm.closeAll()
}

// TestSessionIDsAreUnique: generated identifiers do not collide.
func TestSessionIDsAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate session id %s", id)
		}
		seen[id] = struct{}{}
	}
}
