package main

import (
	"sync"
	"testing"
)

const testChannel = "live"
const testKey = "secret"
const wrongKey = "forged"

// assertIdle fails unless the session is waiting (and not playing).
func assertIdle(t *testing.T, s *RTMPSession, label string) {
	t.Helper()
	if !s.isIdling || s.isPlaying {
		t.Fatalf("%s: expected idle (isIdling=true, isPlaying=false), got isIdling=%v isPlaying=%v",
			label, s.isIdling, s.isPlaying)
	}
}

// assertPlaying fails unless the session is receiving the stream.
func assertPlaying(t *testing.T, s *RTMPSession, label string) {
	t.Helper()
	if s.isIdling || !s.isPlaying {
		t.Fatalf("%s: expected playing (isIdling=false, isPlaying=true), got isIdling=%v isPlaying=%v",
			label, s.isIdling, s.isPlaying)
	}
}

func assertChannelGone(t *testing.T, server *RTMPServer, channel string) {
	t.Helper()
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if _, ok := server.channels[channel]; ok {
		t.Fatalf("channel %q should have been cleaned up but still exists", channel)
	}
}

func assertChannelExists(t *testing.T, server *RTMPServer, channel string) *RTMPChannel {
	t.Helper()
	server.mutex.Lock()
	defer server.mutex.Unlock()
	c := server.channels[channel]
	if c == nil {
		t.Fatalf("channel %q should still exist", channel)
	}
	return c
}

// Scenario 1: publisher ends -> all viewers go back to waiting and receive an
// end notification; the same channel can be re-published and the waiting
// viewers (matching key) are attached again.
func TestEndPublishReturnsViewersToIdleAndRepublishReattaches(t *testing.T) {
	server := newTestServer()

	pub, _ := newTestSession(server, testChannel, testKey)
	v1, v1Conn := newTestSession(server, testChannel, testKey)
	v2, v2Conn := newTestSession(server, testChannel, testKey)

	if !startPublish(server, pub, testChannel, testKey, "stream-1") {
		t.Fatal("first publish must succeed")
	}

	if _, err := addPlayer(server, v1); err != nil {
		t.Fatalf("v1 join: %v", err)
	}
	if _, err := addPlayer(server, v2); err != nil {
		t.Fatalf("v2 join: %v", err)
	}
	assertPlaying(t, v1, "v1")
	assertPlaying(t, v2, "v2")

	beforeV1 := v1Conn.bytesWritten()
	beforeV2 := v2Conn.bytesWritten()

	// Publisher finishes cleanly.
	pub.EndPublish(false)

	assertIdle(t, v1, "v1 after end")
	assertIdle(t, v2, "v2 after end")
	if pub.isPublishing {
		t.Fatal("publisher must be marked as not publishing")
	}
	// End notification bytes must have been written to every viewer.
	if v1Conn.bytesWritten() == beforeV1 {
		t.Fatal("v1 did not receive the end notification")
	}
	if v2Conn.bytesWritten() == beforeV2 {
		t.Fatal("v2 did not receive the end notification")
	}

	// Channel survives while idle viewers are waiting, but is not publishing.
	c := assertChannelExists(t, server, testChannel)
	if c.is_publishing {
		t.Fatal("channel must not be publishing after EndPublish")
	}
	if server.isPublishing(testChannel) {
		t.Fatal("isPublishing must report false")
	}

	// A second publisher starts on the same channel; waiting viewers reattach.
	pub2, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub2, testChannel, testKey, "stream-2") {
		t.Fatal("re-publish on the same channel must succeed")
	}
	assertPlaying(t, v1, "v1 after re-publish")
	assertPlaying(t, v2, "v2 after re-publish")

	c = assertChannelExists(t, server, testChannel)
	if !c.is_publishing || c.publisher != pub2.id {
		t.Fatal("channel must track the new publisher")
	}
}

// Scenario 2: a viewer joining in the gap between end and re-publish becomes
// idle instead of being rejected, and is attached on re-publish.
func TestViewerJoinsWhileIdleWaitsAndIsAttachedOnRepublish(t *testing.T) {
	server := newTestServer()

	pub, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub, testChannel, testKey, "stream-1") {
		t.Fatal("publish must succeed")
	}
	pub.EndPublish(false)

	// Joins while no stream is live, but the channel retains the publisher key.
	late, _ := newTestSession(server, testChannel, testKey)
	idle, err := addPlayer(server, late)
	if err != nil {
		t.Fatalf("matching-key viewer must be allowed to wait, got error: %v", err)
	}
	if !idle {
		t.Fatal("viewer joining an idle channel must be idle")
	}
	assertIdle(t, late, "late viewer")

	pub2, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub2, testChannel, testKey, "stream-2") {
		t.Fatal("re-publish must succeed")
	}
	assertPlaying(t, late, "late viewer after re-publish")
}

// Scenario 3: a wrong-key viewer must be rejected, both while live and while
// the channel is idle but still remembers the publisher's key. It must never
// be admitted just because the channel is temporarily idle.
func TestWrongKeyViewerIsRejectedLiveAndIdle(t *testing.T) {
	server := newTestServer()

	pub, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub, testChannel, testKey, "stream-1") {
		t.Fatal("publish must succeed")
	}

	badLive, _ := newTestSession(server, testChannel, wrongKey)
	if _, err := addPlayer(server, badLive); err == nil {
		t.Fatal("wrong-key viewer must be rejected while live")
	}

	// A matching-key viewer keeps the channel alive while the publisher is
	// away, so the channel retains the authoritative key in the idle state.
	waiter, _ := newTestSession(server, testChannel, testKey)
	if _, err := addPlayer(server, waiter); err != nil {
		t.Fatalf("matching-key viewer must join while live: %v", err)
	}
	assertPlaying(t, waiter, "waiter while live")

	pub.EndPublish(false)
	assertIdle(t, waiter, "waiter goes idle when publisher ends")

	badIdle, badIdleConn := newTestSession(server, testChannel, wrongKey)
	if _, err := addPlayer(server, badIdle); err == nil {
		t.Fatal("wrong-key viewer must be rejected while channel is idle")
	}
	if badIdle.isPlaying {
		t.Fatal("rejected session must not be playing")
	}
	// It must not be registered on the channel at all.
	c := assertChannelExists(t, server, testChannel)
	if c.players[badIdle.id] {
		t.Fatal("rejected wrong-key viewer must not be attached to the channel")
	}
	if badIdleConn.isClosed() {
		t.Fatal("AddPlayer rejection must not close the connection by itself")
	}

	// The legitimate waiter is still queued and reattaches on re-publish.
	pub2, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub2, testChannel, testKey, "stream-2") {
		t.Fatal("re-publish must succeed")
	}
	assertPlaying(t, waiter, "legitimate waiter after re-publish")
}

// Scenario 4: a wrong-key viewer that was already waiting from before the key
// became authoritative is rejected and disconnected when the publisher starts.
func TestWaitingWrongKeyViewerKilledOnPublish(t *testing.T) {
	server := newTestServer()

	// Channel created solely by a waiting viewer, no publisher key yet.
	early, earlyConn := newTestSession(server, testChannel, "stale-key")
	idle, err := addPlayer(server, early)
	if err != nil || !idle {
		t.Fatalf("first waiting viewer on a fresh channel must idle, idle=%v err=%v", idle, err)
	}

	pub, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub, testChannel, testKey, "stream-1") {
		t.Fatal("publish must succeed")
	}

	if !earlyConn.isClosed() {
		t.Fatal("waiting viewer with a mismatching key must be disconnected on publish")
	}
	// And it must no longer be referenced by the channel.
	c := assertChannelExists(t, server, testChannel)
	if c.players[early.id] {
		t.Fatal("killed wrong-key viewer must have been removed from the channel")
	}
}

// Scenario 5: repeated end events for the same publishing session notify the
// viewers exactly once and never rewrite the channel state a second time.
func TestEndPublishIsIdempotentAndNotifiesOnce(t *testing.T) {
	server := newTestServer()

	pub, _ := newTestSession(server, testChannel, testKey)
	v, vConn := newTestSession(server, testChannel, testKey)

	startPublish(server, pub, testChannel, testKey, "stream-1")
	if _, err := addPlayer(server, v); err != nil {
		t.Fatalf("join: %v", err)
	}
	assertPlaying(t, v, "viewer")

	// First end: notification delivered, viewer goes idle.
	pub.EndPublish(false)
	assertIdle(t, v, "viewer after first end")
	afterFirst := vConn.bytesWritten()

	// A duplicate end (e.g. deleteStream then TCP close) must be a no-op.
	pub.EndPublish(true)
	pub.EndPublish(false)
	if vConn.bytesWritten() != afterFirst {
		t.Fatal("duplicate EndPublish must not send additional notifications")
	}
	assertIdle(t, v, "viewer stays idle")

	// The channel state must still be the post-end state, not re-published.
	c := assertChannelExists(t, server, testChannel)
	if c.is_publishing {
		t.Fatal("duplicate EndPublish must not flip the channel back to publishing")
	}
}

// Scenario 6: once the last publisher and last viewer are gone, the channel
// record is deleted; publishing again must not report "already publishing".
func TestChannelCleanedUpWhenEmptyAndRepublishSucceeds(t *testing.T) {
	server := newTestServer()

	pub, _ := newTestSession(server, testChannel, testKey)
	v, _ := newTestSession(server, testChannel, testKey)

	startPublish(server, pub, testChannel, testKey, "stream-1")
	if _, err := addPlayer(server, v); err != nil {
		t.Fatalf("join: %v", err)
	}

	// Publisher leaves first; viewer keeps the channel alive while waiting.
	pub.EndPublish(false)
	assertChannelExists(t, server, testChannel)

	// Last viewer leaves -> channel must be cleaned up.
	server.RemovePlayer(testChannel, v.key, v)
	assertChannelGone(t, server, testChannel)

	// A fresh publish must succeed (no residual "already publishing" state).
	pub2, _ := newTestSession(server, testChannel, testKey)
	if !startPublish(server, pub2, testChannel, testKey, "stream-2") {
		t.Fatal("publish after full cleanup must succeed")
	}
	if !server.isPublishing(testChannel) {
		t.Fatal("channel must be publishing again")
	}

	// And when that publisher leaves with no viewers, it is cleaned up too.
	pub2.EndPublish(false)
	assertChannelGone(t, server, testChannel)
}

// Scenario 6b: RemovePlayer on an already-deleted channel must be safe (the
// interleaving where cleanup wins the race with the disconnect path).
func TestRemovePlayerOnMissingChannelIsSafe(t *testing.T) {
	server := newTestServer()
	s, _ := newTestSession(server, testChannel, testKey)
	s.isPlaying = true

	// Channel does not exist at all.
	server.RemovePlayer(testChannel, testKey, s)
	if s.isIdling || s.isPlaying {
		t.Fatal("session flags must be reset even when the channel is gone")
	}
	assertChannelGone(t, server, testChannel)
}

// Scenario 7: concurrent publish-end vs. viewer joins must never produce a
// session marked playing that is not attached to a live publisher.
func TestConcurrentEndPublishAndJoins(t *testing.T) {
	server := newTestServer()

	for round := 0; round < 50; round++ {
		channel := "live-round"
		server.channels = make(map[string]*RTMPChannel)

		pub, _ := newTestSession(server, channel, testKey)
		startPublish(server, pub, channel, testKey, "stream")

		const joiners = 20
		var wg sync.WaitGroup
		viewers := make([]*RTMPSession, joiners)
		for i := 0; i < joiners; i++ {
			v, _ := newTestSession(server, channel, testKey)
			viewers[i] = v
		}

		wg.Add(joiners + 1)
		go func() {
			defer wg.Done()
			pub.EndPublish(false)
		}()
		for i := 0; i < joiners; i++ {
			go func(v *RTMPSession) {
				defer wg.Done()
				_, _ = addPlayer(server, v)
			}(viewers[i])
		}
		wg.Wait()

		// Invariant: every still-attached viewer is either idle or playing on
		// a live publisher; never playing on a non-publishing channel.
		server.mutex.Lock()
		c := server.channels[channel]
		if c != nil {
			if c.is_publishing == false {
				for sid := range c.players {
					s := server.sessions[sid]
					if s != nil && s.isPlaying {
						server.mutex.Unlock()
						t.Fatalf("round %d: viewer %d marked playing on non-publishing channel", round, sid)
					}
				}
			}
		}
		server.mutex.Unlock()

		// Re-publish must always succeed afterwards.
		pub2, _ := newTestSession(server, channel, testKey)
		if !startPublish(server, pub2, channel, testKey, "stream-2") {
			t.Fatalf("round %d: re-publish blocked by residual publishing state", round)
		}
		pub2.EndPublish(false)
	}
}

// Scenario 8: concurrent viewer disconnects vs. channel cleanup must leave no
// dangling references and always delete the empty channel exactly once.
func TestConcurrentLeavesAndCleanup(t *testing.T) {
	server := newTestServer()

	for round := 0; round < 50; round++ {
		channel := "leave-round"

		pub, _ := newTestSession(server, channel, testKey)
		startPublish(server, pub, channel, testKey, "stream")

		const n = 16
		viewers := make([]*RTMPSession, n)
		for i := 0; i < n; i++ {
			v, _ := newTestSession(server, channel, testKey)
			if _, err := addPlayer(server, v); err != nil {
				t.Fatalf("round %d join %d: %v", round, i, err)
			}
			viewers[i] = v
		}

		var wg sync.WaitGroup
		wg.Add(n + 1)
		go func() {
			defer wg.Done()
			pub.EndPublish(false)
		}()
		for i := 0; i < n; i++ {
			go func(v *RTMPSession) {
				defer wg.Done()
				server.RemovePlayer(channel, v.key, v)
			}(viewers[i])
		}
		wg.Wait()

		assertChannelGone(t, server, channel)

		// No session should still believe it is playing.
		for _, v := range viewers {
			if v.isPlaying || v.isIdling {
				t.Fatalf("round %d: disconnected viewer retains stale state playing=%v idling=%v",
					round, v.isPlaying, v.isIdling)
			}
		}
	}
}

// Scenario 9: OnClose (connection drop) tears down both a publisher and a
// player on the same session path without leaving channel residue.
func TestOnCloseReleasesPublisherAndPlayer(t *testing.T) {
	server := newTestServer()

	// Publishing session dropping its connection.
	pub, _ := newTestSession(server, testChannel, testKey)
	viewer, _ := newTestSession(server, testChannel, testKey)
	startPublish(server, pub, testChannel, testKey, "stream")
	if _, err := addPlayer(server, viewer); err != nil {
		t.Fatalf("join: %v", err)
	}

	pub.publishStreamId = 2
	pub.OnClose()
	if pub.isPublishing {
		t.Fatal("OnClose must end publishing")
	}
	assertIdle(t, viewer, "viewer after publisher disconnect")

	viewer.playStreamId = 1
	viewer.OnClose()
	assertChannelGone(t, server, testChannel)
}

// Scenario 10: full lifecycle driven twice in sequence on one channel, proving
// the state machine is reusable and repeatable.
func TestFullLifecycleRepeats(t *testing.T) {
	server := newTestServer()

	for iteration := 0; iteration < 3; iteration++ {
		pub, _ := newTestSession(server, testChannel, testKey)
		v, _ := newTestSession(server, testChannel, testKey)

		if !startPublish(server, pub, testChannel, testKey, "stream") {
			t.Fatalf("iteration %d: publish failed", iteration)
		}
		if _, err := addPlayer(server, v); err != nil {
			t.Fatalf("iteration %d: join failed: %v", iteration, err)
		}
		assertPlaying(t, v, "viewer")

		pub.EndPublish(false)
		assertIdle(t, v, "viewer")

		server.RemovePlayer(testChannel, v.key, v)
		assertChannelGone(t, server, testChannel)
	}
}
