package rtmp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
)

func TestPublishAdmissionFailureRestoresStreamStateAndEvents(t *testing.T) {
	hub, bus := newTestHub()
	streamKey := "live/rollback-admission"
	publisherID := publisherIDFor(streamKey)
	stopCount := countEvents(bus, core.EventPublishStop)
	release := saturateRTMPPublishLifecycle(t, bus, &core.EventContext{StreamKey: streamKey, PublisherID: publisherID})
	defer release()

	handler, peer := newPublishTestHandler(t, hub, bus, "live")
	defer peer.Close()

	err := handler.onPublish(publishVals("rollback-admission"))
	if !errors.Is(err, core.ErrAsyncBackpressure) {
		t.Fatalf("onPublish error = %v, want ErrAsyncBackpressure", err)
	}
	if handler.isPublisher || handler.publisher != nil || handler.streamKey != "" {
		t.Fatal("handler retained publish state after a failed admission")
	}
	// Admission is atomic: a refused start means neither the stream nor a
	// lifecycle event was left behind, so no stop event may be emitted.
	if stream, ok := hub.Find(streamKey); ok && (stream.Publisher() != nil || stream.State() == core.StreamStatePublishing) {
		t.Fatal("failed admission left the stream publishing")
	}
	if got := stopCount(); got != 0 {
		t.Fatalf("publish stop events after refused admission = %d, want 0", got)
	}
}

func TestPublishAdmissionFailureKeepsPreexistingStream(t *testing.T) {
	hub, bus := newTestHub()
	streamKey := "live/rollback-existing"
	existing, err := hub.GetOrCreate(streamKey)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	// A subscriber makes the stream non-discardable: rollback must restore it
	// rather than retire it out from under the subscriber.
	if err := existing.AddSubscriber("rtmp"); err != nil {
		t.Fatalf("add subscriber: %v", err)
	}
	defer existing.RemoveSubscriber("rtmp")

	stopCount := countEvents(bus, core.EventPublishStop)
	saturated := saturateRTMPPublishLifecycle(t, bus, &core.EventContext{StreamKey: streamKey, PublisherID: publisherIDFor(streamKey)})
	defer saturated()

	handler, peer := newPublishTestHandler(t, hub, bus, "live")
	defer peer.Close()
	if err := handler.onPublish(publishVals("rollback-existing")); !errors.Is(err, core.ErrAsyncBackpressure) {
		t.Fatalf("onPublish error = %v, want ErrAsyncBackpressure", err)
	}
	stream, ok := hub.Find(streamKey)
	if !ok || stream.InstanceID() != existing.InstanceID() {
		t.Fatal("rollback replaced or retired a stream that still had consumers")
	}
	if stream.Publisher() != nil {
		t.Fatal("failed admission left a publisher on the preexisting stream")
	}
	if stream.State() == core.StreamStatePublishing {
		t.Fatal("failed admission left the preexisting stream publishing")
	}
	if got := stopCount(); got != 0 {
		t.Fatalf("publish stop events after refused admission = %d, want 0", got)
	}
}

func TestPublishSecondPublishOnSameConnectionRejected(t *testing.T) {
	hub, bus := newTestHub()
	handler, peer := newPublishTestHandler(t, hub, bus, "live")
	defer peer.Close()

	if err := handler.onPublish(publishVals("first")); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	first := handler.publisher
	if err := handler.onPublish(publishVals("second")); err == nil {
		t.Fatal("second publish on the same connection was accepted")
	}
	if handler.publisher != first || handler.streamKey != "live/first" {
		t.Fatal("second publish replaced the tracked publisher")
	}
	if _, ok := hub.Find("live/second"); ok {
		t.Fatal("rejected publish created a stream")
	}
	if stream, ok := hub.Find("live/first"); !ok || stream.Publisher() != first {
		t.Fatal("accepted publish lost its stream publisher")
	}
}

func TestPublishCleanupIsIdempotentUnderConcurrentDisconnect(t *testing.T) {
	hub, bus := newTestHub()
	stopCount := countEvents(bus, core.EventPublishStop)
	handler, peer := newPublishTestHandler(t, hub, bus, "live")
	if err := handler.onPublish(publishVals("concurrent")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	stream, ok := hub.Find("live/concurrent")
	if !ok {
		t.Fatal("stream missing")
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.cleanup()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = peer.Close()
	}()
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for stopCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Give any second emission a chance to surface before asserting exactly one.
	time.Sleep(100 * time.Millisecond)
	if got := stopCount(); got != 1 {
		t.Fatalf("publish stop events = %d, want exactly 1", got)
	}
	if stream.Publisher() != nil {
		t.Fatal("concurrent cleanup left a ghost publisher")
	}
}

// countEvents counts asynchronous deliveries of one event type.
func countEvents(bus *core.EventBus, event core.EventType) func() int {
	var mu sync.Mutex
	var count int
	bus.Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

// publisherIDFor predicts the identifier the next RTMP publisher allocated for
// streamKey will receive.
func publisherIDFor(streamKey string) string {
	return fmt.Sprintf("rtmp-pub-%s-%d", streamKey, publisherSequence.Load()+1)
}

func publishVals(name string) []any {
	return []any{"publish", float64(0), nil, name}
}

// newPublishTestHandler builds a handler bound to an in-memory pipe whose peer
// is drained so onStatus writes never block.
func newPublishTestHandler(t *testing.T, hub *core.StreamHub, bus *core.EventBus, app string) (*Handler, net.Conn) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()
	handler := NewHandler(serverConn, hub, bus, 4096, nil)
	handler.app = app
	return handler, clientConn
}
