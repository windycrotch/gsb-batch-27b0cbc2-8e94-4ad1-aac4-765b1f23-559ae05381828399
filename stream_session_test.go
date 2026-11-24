package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/sirupsen/logrus"
)

// This file covers the play session lifecycle: concurrent playback, source
// reconnect, viewer registration, slow viewer cleanup and idempotent stop.
// Everything runs against local fakes injected through streamSourceDialer and
// newWebRTCMuxer; no RTSP server, STUN server or external network is used.

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	log.SetLevel(logrus.PanicLevel)
	m.Run()
}

const (
	testOffer = "AAAA" // opaque to the fake muxer
	testURL   = "rtsp://fake.invalid/stream"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeSource is a controllable stream source. Packets/RTP/Signals are real
// channels owned by the test, so it can publish media, announce codec updates
// and simulate an upstream disconnect by closing the packet channel.
type fakeSource struct {
	mu        sync.Mutex
	codecs    []av.CodecData
	sdp       []byte
	waitCodec bool
	packets   chan *av.Packet
	rtp       chan *[]byte
	signals   chan int

	closeOnce sync.Once
	closed    chan struct{}
	dropOnce  sync.Once
}

func newFakeSource(codecs ...av.CodecData) *fakeSource {
	return &fakeSource{
		codecs:  codecs,
		sdp:     []byte("v=0"),
		packets: make(chan *av.Packet, 4096),
		rtp:     make(chan *[]byte, 4096),
		signals: make(chan int, 8),
		closed:  make(chan struct{}),
	}
}

func (f *fakeSource) Codecs() []av.CodecData {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]av.CodecData(nil), f.codecs...)
}

func (f *fakeSource) SetCodecs(codecs ...av.CodecData) {
	f.mu.Lock()
	f.codecs = append([]av.CodecData(nil), codecs...)
	f.mu.Unlock()
}

func (f *fakeSource) SDP() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.sdp...)
}

func (f *fakeSource) WaitCodec() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitCodec
}

func (f *fakeSource) Packets() <-chan *av.Packet { return f.packets }
func (f *fakeSource) RTP() <-chan *[]byte        { return f.rtp }
func (f *fakeSource) Signals() <-chan int        { return f.signals }
func (f *fakeSource) Close()                     { f.closeOnce.Do(func() { close(f.closed) }) }

// drop simulates the upstream connection dying mid stream.
func (f *fakeSource) drop() { f.dropOnce.Do(func() { close(f.packets) }) }

// fakeMuxer records what a viewer would have sent and whether it was closed.
type fakeMuxer struct {
	mu      sync.Mutex
	packets []av.Packet
	closed  bool
	writeEr error
}

func (m *fakeMuxer) WritePacket(pkt av.Packet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeEr != nil {
		return m.writeEr
	}
	m.packets = append(m.packets, pkt)
	return nil
}

func (m *fakeMuxer) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

func (m *fakeMuxer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.packets)
}

func (m *fakeMuxer) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// muxerRecorder creates one fake muxer per play call and keeps them in order.
type muxerRecorder struct {
	mu     sync.Mutex
	muxers []*fakeMuxer
	// onHeader runs inside the muxer factory, before registration. It lets a
	// test simulate a codec change (or a stop) racing with registration.
	onHeader func()
	// headerErr forces WriteHeader to fail.
	headerErr error
}

func (r *muxerRecorder) install(t *testing.T) {
	t.Helper()
	old := newWebRTCMuxer
	newWebRTCMuxer = func(codecs []av.CodecData, offer string) (webrtcMuxer, string, error) {
		if r.onHeader != nil {
			r.onHeader()
		}
		if r.headerErr != nil {
			return nil, "", r.headerErr
		}
		m := &fakeMuxer{}
		r.mu.Lock()
		r.muxers = append(r.muxers, m)
		r.mu.Unlock()
		return m, "ZmFrZS1hbnN3ZXI=", nil
	}
	t.Cleanup(func() { newWebRTCMuxer = old })
}

func (r *muxerRecorder) last(t *testing.T) *fakeMuxer {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.muxers) == 0 {
		t.Fatalf("no muxer was created")
	}
	return r.muxers[len(r.muxers)-1]
}

func (r *muxerRecorder) all() []*fakeMuxer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeMuxer(nil), r.muxers...)
}

// dialerFunc lets a test own the dial behaviour. It counts calls.
type dialerFunc struct {
	mu   sync.Mutex
	fn   func(call int) (streamSource, error)
	call int
}

func (d *dialerFunc) install(t *testing.T) {
	t.Helper()
	old := streamSourceDialer
	streamSourceDialer = func(opt StreamST) (streamSource, error) {
		d.mu.Lock()
		d.call++
		n := d.call
		d.mu.Unlock()
		return d.fn(n)
	}
	t.Cleanup(func() { streamSourceDialer = old })
}

func (d *dialerFunc) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.call
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func codecH264(tag byte) av.CodecData {
	return h264parser.CodecData{Record: []byte{0x01, tag}}
}

func keyPacket(ts time.Duration) *av.Packet {
	return &av.Packet{IsKeyFrame: true, Idx: 0, Time: ts, Duration: 40 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65}}
}

func deltaPacket(ts time.Duration) *av.Packet {
	return &av.Packet{IsKeyFrame: false, Idx: 0, Time: ts, Duration: 40 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41}}
}

// shortenTimings makes reconnect checks fast for tests and restores them after.
// timingOverrides customises collector/viewer timings for one test. Zero
// fields keep the value chosen by the test defaults.
type timingOverrides struct {
	reconnectDelay time.Duration
	keyTestTimeout time.Duration
	checkClients   time.Duration
	noVideoTimeout time.Duration
}

// shortenTimings makes reconnect checks fast for tests. It must be called
// before cleanupStreams so that its restore cleanup (registered first) runs
// only after the session has been stopped and no collector is left reading the
// package level timing variables.
func shortenTimings(t *testing.T) {
	t.Helper()
	applyTimings(t, timingOverrides{})
}

// applyTimings installs fast test timings plus any overrides and restores the
// original values when the test ends.
func applyTimings(t *testing.T, o timingOverrides) {
	t.Helper()
	oldDelay, oldKey, oldCheck, oldNoVideo := streamReconnectDelay, streamKeyTestTimeout, streamCheckClientsInterval, streamViewerNoVideoTimeout
	streamReconnectDelay = 5 * time.Millisecond
	streamKeyTestTimeout = 2 * time.Second
	streamCheckClientsInterval = 2 * time.Second
	streamViewerNoVideoTimeout = 2 * time.Second
	if o.reconnectDelay > 0 {
		streamReconnectDelay = o.reconnectDelay
	}
	if o.keyTestTimeout > 0 {
		streamKeyTestTimeout = o.keyTestTimeout
	}
	if o.checkClients > 0 {
		streamCheckClientsInterval = o.checkClients
	}
	if o.noVideoTimeout > 0 {
		streamViewerNoVideoTimeout = o.noVideoTimeout
	}
	t.Cleanup(func() {
		streamReconnectDelay, streamKeyTestTimeout, streamCheckClientsInterval, streamViewerNoVideoTimeout = oldDelay, oldKey, oldCheck, oldNoVideo
	})
}

// cleanupStreams stops every session a test created and then removes the
// session entries, so tests stay isolated when the suite is run more than once
// in the same process.
func cleanupStreams(t *testing.T, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			_ = Storage.StreamChannelStop(id)
			Storage.mutex.Lock()
			delete(Storage.Streams, id)
			Storage.mutex.Unlock()
		}
	})
}

// sessionCount returns how many sessions are currently registered.
func sessionCount() int {
	Storage.mutex.RLock()
	defer Storage.mutex.RUnlock()
	return len(Storage.Streams)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// streamSnapshot is a locked copy of the mutable session state a test needs.
type streamSnapshot struct {
	runLock bool
	stop    chan struct{}
	signals chan int
	done    chan struct{}
	clients int
	codecs  int
	status  int
}

func streamSnapshotOf(t *testing.T, id string) streamSnapshot {
	t.Helper()
	Storage.mutex.RLock()
	defer Storage.mutex.RUnlock()
	s, ok := Storage.Streams[id]
	if !ok {
		t.Fatalf("stream %q missing from storage", id)
	}
	return streamSnapshot{
		runLock: s.runLock,
		stop:    s.stop,
		signals: s.signals,
		done:    s.done,
		clients: len(s.clients),
		codecs:  len(s.codecs),
		status:  s.Status,
	}
}

func liveClientCount(id string) int {
	Storage.mutex.RLock()
	defer Storage.mutex.RUnlock()
	s, ok := Storage.Streams[id]
	if !ok {
		return 0
	}
	return len(s.clients)
}

// streamStatus reads the session status under the storage lock.
func streamStatus(id string) int {
	Storage.mutex.RLock()
	defer Storage.mutex.RUnlock()
	s, ok := Storage.Streams[id]
	if !ok {
		return -1
	}
	return s.Status
}

// ---------------------------------------------------------------------------
// A1 / A8: concurrent play must share exactly one collection session.
// ---------------------------------------------------------------------------

func TestConcurrentPlaySharesOneCollectionSession(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/concurrent"

	src := newFakeSource(codecH264('a'))
	dial := &dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}
	dial.install(t)
	rec := &muxerRecorder{}
	rec.install(t)
	cleanupStreams(t, url)

	const callers = 24
	results := make([]string, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = ServerStreamWebRTC(url, testOffer)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, res := range results {
		if res == "" {
			t.Fatalf("caller %d got an empty answer", i)
		}
		if IsStreamError(res) {
			t.Fatalf("caller %d got an error answer: %q", i, res)
		}
	}
	if got := dial.calls(); got != 1 {
		t.Fatalf("expected exactly one collection connection, got %d dials", got)
	}
	if got := liveClientCount(url); got != callers {
		t.Fatalf("expected %d registered viewers, got %d", callers, got)
	}

	Storage.mutex.RLock()
	streams := len(Storage.Streams)
	Storage.mutex.RUnlock()
	if streams != 1 {
		t.Fatalf("expected only the one session this test created, got %d", streams)
	}

	// A later duplicate request must reuse the same session object.
	infoBefore, err := Storage.StreamChannelInfo(url)
	if err != nil {
		t.Fatalf("StreamChannelInfo: %v", err)
	}
	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("sequential duplicate play failed: %q", res)
	}
	infoAfter, _ := Storage.StreamChannelInfo(url)
	if infoBefore != infoAfter {
		t.Fatal("duplicate play replaced the session instead of reusing it")
	}
	if got := dial.calls(); got != 1 {
		t.Fatalf("duplicate play started another collector: %d dials", got)
	}

	// Stopping detaches every viewer; all viewer goroutines must exit.
	if err := Storage.StreamChannelStop(url); err != nil {
		t.Fatalf("StreamChannelStop: %v", err)
	}
	waitFor(t, "all viewers detached", func() bool { return liveClientCount(url) == 0 })
	waitFor(t, "all muxers closed", func() bool {
		for _, m := range rec.all() {
			if !m.isClosed() {
				return false
			}
		}
		return true
	})
	if s := streamSnapshotOf(t, url); s.runLock {
		t.Fatal("run lock still set after stop")
	}
}

// ---------------------------------------------------------------------------
// A2: failures are recognisable, and registration failures reach the caller.
// ---------------------------------------------------------------------------

func TestPlayFailuresAreRecognisableAndRegistrationFailureIsSurfaced(t *testing.T) {
	shortenTimings(t)

	t.Run("empty stream identity", func(t *testing.T) {
		res := ServerStreamWebRTC("   ", testOffer)
		if res == "" || !IsStreamError(res) {
			t.Fatalf("expected a recognisable error, got %q", res)
		}
	})

	t.Run("empty offer", func(t *testing.T) {
		const url = "rtsp://fake.invalid/empty-offer"
		src := newFakeSource(codecH264('a'))
		(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
		cleanupStreams(t, url)
		res := ServerStreamWebRTC(url, "")
		if res == "" || !IsStreamError(res) {
			t.Fatalf("expected a recognisable error, got %q", res)
		}
	})

	t.Run("write header failure", func(t *testing.T) {
		const url = "rtsp://fake.invalid/header-failure"
		src := newFakeSource(codecH264('a'))
		(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
		rec := &muxerRecorder{headerErr: fmt.Errorf("boom header")}
		rec.install(t)
		cleanupStreams(t, url)

		res := ServerStreamWebRTC(url, testOffer)
		if res == "" || !IsStreamError(res) || !strings.Contains(res, "boom header") {
			t.Fatalf("expected a recognisable header error, got %q", res)
		}
		if got := liveClientCount(url); got != 0 {
			t.Fatalf("viewer registered despite header failure: %d", got)
		}
	})

	t.Run("registration failure is returned not swallowed", func(t *testing.T) {
		const url = "rtsp://fake.invalid/registration-failure"
		src := newFakeSource(codecH264('a'))
		(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
		rec := &muxerRecorder{}
		// Stop the session between building the muxer and registering the
		// viewer: registration must fail and the caller must see it.
		rec.onHeader = func() { _ = Storage.StreamChannelStop(url) }
		rec.install(t)
		cleanupStreams(t, url)

		res := ServerStreamWebRTC(url, testOffer)
		if res == "" || !IsStreamError(res) {
			t.Fatalf("registration failure was not surfaced, got %q", res)
		}
		if !strings.Contains(res, ErrorStreamChannelStopped.Error()) {
			t.Fatalf("expected stopped-session error, got %q", res)
		}
		if got := liveClientCount(url); got != 0 {
			t.Fatalf("viewer registered on a stopped session: %d", got)
		}
	})

	t.Run("registration on stopped session fails", func(t *testing.T) {
		const url = "rtsp://fake.invalid/stopped-registration"
		src := newFakeSource(codecH264('a'))
		(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
		cleanupStreams(t, url)
		if _, _, err := Storage.StreamChannelGetOrCreate(url, StreamST{URL: url}); err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}
		if err := Storage.StreamChannelStop(url); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if _, err := Storage.ClientAddChecked(url, WEBRTC, 0); err != ErrorStreamChannelStopped {
			t.Fatalf("expected ErrorStreamChannelStopped, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// A3: viewers survive a reconnect and stale codec info is never reused.
// ---------------------------------------------------------------------------

func TestReconnectKeepsViewerAndInvalidatesStaleCodecs(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/reconnect"

	codecA := codecH264('a')
	codecB := codecH264('b')
	src1 := newFakeSource(codecA)
	src2 := newFakeSource(codecB)

	dial2Started := make(chan struct{})
	release2 := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release2) }) })
	dial := &dialerFunc{fn: func(call int) (streamSource, error) {
		switch call {
		case 1:
			return src1, nil
		case 2:
			close(dial2Started)
			<-release2
			return src2, nil
		default:
			return src2, nil
		}
	}}
	dial.install(t)
	rec := &muxerRecorder{}
	rec.install(t)
	cleanupStreams(t, url)

	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("initial play failed: %q", res)
	}
	mux := rec.last(t)
	waitFor(t, "viewer registered", func() bool { return liveClientCount(url) == 1 })

	_, epochA, err := Storage.StreamChannelCodecsEpoch(url)
	if err != nil {
		t.Fatalf("codecs before reconnect: %v", err)
	}

	// First connection delivers a keyframe, then a delta frame.
	src1.packets <- keyPacket(0)
	waitFor(t, "first keyframe delivered", func() bool { return mux.count() >= 1 })
	src1.packets <- deltaPacket(40 * time.Millisecond)
	waitFor(t, "delta frame delivered", func() bool { return mux.count() >= 2 })

	// Source drops. The viewer stays attached but must wait for a keyframe.
	src1.drop()
	waitFor(t, "reconnect dial started", func() bool {
		select {
		case <-dial2Started:
			return true
		default:
			return false
		}
	})
	waitFor(t, "viewer survived the reconnect", func() bool { return liveClientCount(url) == 1 })

	// While disconnected no codec information may be handed out.
	if codecs := streamCodecsOrNil(url); len(codecs) != 0 {
		t.Fatalf("stale codecs still published while offline: %v", codecs)
	}

	releaseOnce.Do(func() { close(release2) })
	waitFor(t, "reconnected codecs published", func() bool {
		codecs, _, err := Storage.StreamChannelCodecsEpoch(url)
		if err != nil || len(codecs) == 0 {
			return false
		}
		return fmt.Sprintf("%v", codecs) == fmt.Sprintf("%v", []av.CodecData{codecB})
	})

	codecsB, epochB, err := Storage.StreamChannelCodecsEpoch(url)
	if err != nil {
		t.Fatalf("codecs after reconnect: %v", err)
	}
	if len(codecsB) != 1 || codecsB[0].Type() != av.H264 {
		t.Fatalf("unexpected codecs after reconnect: %v", codecsB)
	}
	if epochB == epochA {
		t.Fatal("codec epoch did not change across the reconnect")
	}

	// The attached viewer receives the next keyframe of the new connection,
	// but never a delta frame before it.
	before := mux.count()
	src2.packets <- deltaPacket(80 * time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if got := mux.count(); got != before {
		t.Fatalf("viewer received a delta frame before a keyframe after reconnect (count %d -> %d)", before, got)
	}
	src2.packets <- keyPacket(120 * time.Millisecond)
	waitFor(t, "keyframe after reconnect delivered", func() bool { return mux.count() > before })

	// A viewer prepared against the old codecs must not be registered with the
	// new ones.
	if _, err := Storage.ClientAddChecked(url, WEBRTC, epochA); err != ErrorStreamChannelCodecsChanged {
		t.Fatalf("expected stale-codec registration to fail, got %v", err)
	}
	reg, err := Storage.ClientAddChecked(url, WEBRTC, epochB)
	if err != nil {
		t.Fatalf("registration with current codecs failed: %v", err)
	}
	Storage.ClientDelete(url, reg.id)
}

// streamCodecsOrNil reads the raw published codecs without waiting.
func streamCodecsOrNil(id string) []av.CodecData {
	Storage.mutex.RLock()
	defer Storage.mutex.RUnlock()
	s, ok := Storage.Streams[id]
	if !ok {
		return nil
	}
	return append([]av.CodecData(nil), s.codecs...)
}

// ---------------------------------------------------------------------------
// A4: a slow viewer is detached; other viewers keep receiving.
// ---------------------------------------------------------------------------

func TestSlowViewerIsDetachedWithoutBlockingOthers(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/slow-viewer"

	src := newFakeSource(codecH264('a'))
	(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
	cleanupStreams(t, url)

	if _, _, err := Storage.StreamChannelGetOrCreate(url, StreamST{URL: url}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	waitFor(t, "codecs available", func() bool { return len(streamCodecsOrNil(url)) > 0 })

	slow, err := Storage.ClientAddChecked(url, WEBRTC, 0)
	if err != nil {
		t.Fatalf("slow ClientAddChecked: %v", err)
	}
	// Give only the slow viewer a tiny queue so the burst below overflows it
	// deterministically while the healthy viewer (default limit) keeps every
	// packet. This keeps the test independent of goroutine scheduling.
	Storage.mutex.Lock()
	Storage.Streams[url].clients[slow.id].queueLimit = 2
	Storage.mutex.Unlock()

	fast, err := Storage.ClientAddChecked(url, WEBRTC, 0)
	if err != nil {
		t.Fatalf("fast ClientAddChecked: %v", err)
	}

	var fastCount int
	var fastMu sync.Mutex
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		for {
			select {
			case <-fast.stop:
				return
			case <-fast.avPackets:
				fastMu.Lock()
				fastCount++
				fastMu.Unlock()
			}
		}
	}()

	// The slow viewer never reads its channel. Feed more packets than its
	// queue limit: it must be detached rather than block the collector.
	const pushes = 16
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < pushes; i++ {
			Storage.StreamChannelCast(url, keyPacket(time.Duration(i)*40*time.Millisecond))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast blocked on a slow viewer")
	}

	waitFor(t, "slow viewer detached", func() bool {
		select {
		case <-slow.stop:
			return true
		default:
			return false
		}
	})
	// The fast viewer always pulls from its queue: only it may remain, and it
	// must have received the packets the slow viewer missed.
	waitFor(t, "only the fast viewer left", func() bool { return liveClientCount(url) == 1 })
	waitFor(t, "healthy viewer received packets", func() bool {
		fastMu.Lock()
		defer fastMu.Unlock()
		return fastCount > 0
	})

	// Deleting the detached viewer again must be a no-op.
	Storage.ClientDelete(url, slow.id)
	Storage.ClientDelete(url, slow.id)
	if got := liveClientCount(url); got != 1 {
		t.Fatalf("unexpected viewer count after redundant delete: %d", got)
	}

	Storage.ClientDelete(url, fast.id)
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("fast viewer goroutine did not stop after detach")
	}
}

// TestReconnectConnectFailureKeepsExistingViewers checks that a failed
// reconnect attempt (dial error) does not detach viewers that are already
// attached: they wait for a later successful connection.
func TestReconnectConnectFailureKeepsExistingViewers(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/reconnect-dial-failure"

	src1 := newFakeSource(codecH264('a'))
	src3 := newFakeSource(codecH264('c'))
	secondDialFailed := make(chan struct{}, 1)
	var failOnce sync.Once
	dial := &dialerFunc{fn: func(call int) (streamSource, error) {
		switch call {
		case 1:
			return src1, nil
		case 2:
			failOnce.Do(func() { secondDialFailed <- struct{}{} })
			return nil, fmt.Errorf("simulated dial failure")
		default:
			return src3, nil
		}
	}}
	dial.install(t)
	rec := &muxerRecorder{}
	rec.install(t)
	cleanupStreams(t, url)

	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("initial play failed: %q", res)
	}
	waitFor(t, "viewer registered", func() bool { return liveClientCount(url) == 1 })

	src1.drop()
	select {
	case <-secondDialFailed:
	case <-time.After(5 * time.Second):
		t.Fatal("second dial was not attempted")
	}
	// The failed attempt must not remove the attached viewer, and must keep
	// retrying until a source is available.
	waitFor(t, "viewer kept across failed dial", func() bool { return liveClientCount(url) == 1 })
	waitFor(t, "third dial succeeded", func() bool { return dial.calls() >= 3 })
	waitFor(t, "reconnected codecs published", func() bool { return len(streamCodecsOrNil(url)) > 0 })
	if got := liveClientCount(url); got != 1 {
		t.Fatalf("viewer lost after failed reconnect: %d", got)
	}
	if got := streamStatus(url); got != ONLINE {
		t.Fatalf("session should be online after the third dial, got %d", got)
	}
}

// TestViewerWithoutVideoTimesOutAndIsRemoved checks that a viewer that never
// receives a keyframe is removed from the session (and its muxer released)
// when the no-video timeout expires. The session itself stays up.
func TestViewerWithoutVideoTimesOutAndIsRemoved(t *testing.T) {
	// Keep the collector's own "no keyframe" watchdog far away so only the
	// viewer timeout is under test.
	applyTimings(t, timingOverrides{keyTestTimeout: time.Minute, noVideoTimeout: 150 * time.Millisecond})
	const url = "rtsp://fake.invalid/no-video-timeout"

	src := newFakeSource(codecH264('a'))
	(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
	rec := &muxerRecorder{}
	rec.install(t)
	cleanupStreams(t, url)

	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("play failed: %q", res)
	}
	waitFor(t, "viewer registered", func() bool { return liveClientCount(url) == 1 })

	waitFor(t, "viewer removed after timeout", func() bool { return liveClientCount(url) == 0 })
	waitFor(t, "muxer closed after timeout", func() bool { return rec.last(t).isClosed() })
	if got := streamStatus(url); got != ONLINE {
		t.Fatalf("session should still be online, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// A5 / A6: stop is idempotent, clean and restartable; delete is idempotent.
// ---------------------------------------------------------------------------

func TestStopIsIdempotentCleanAndRestartable(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/stop-restart"

	// Snapshot the goroutine count before any session goroutine exists, so a
	// leak in the stop path is detectable at the end of the test.
	baseGoroutines := runtime.NumGoroutine()

	src1 := newFakeSource(codecH264('a'))
	src2 := newFakeSource(codecH264('b'))
	dial := &dialerFunc{fn: func(call int) (streamSource, error) {
		if call == 1 {
			return src1, nil
		}
		return src2, nil
	}}
	dial.install(t)
	rec := &muxerRecorder{}
	rec.install(t)
	cleanupStreams(t, url)

	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("initial play failed: %q", res)
	}
	waitFor(t, "viewer registered", func() bool { return liveClientCount(url) == 1 })

	// Two concurrent stops must neither panic nor double close.
	var wg sync.WaitGroup
	panics := make(chan interface{}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					panics <- p
				}
			}()
			if err := Storage.StreamChannelStop(url); err != nil {
				panics <- err
			}
		}()
	}
	wg.Wait()
	close(panics)
	for p := range panics {
		t.Fatalf("stop reported: %v", p)
	}

	// The collector goroutine and the viewer goroutine must both exit.
	waitFor(t, "collector source closed", func() bool {
		select {
		case <-src1.closed:
			return true
		default:
			return false
		}
	})
	waitFor(t, "viewer detached", func() bool { return liveClientCount(url) == 0 })
	waitFor(t, "muxer closed", func() bool { return rec.last(t).isClosed() })

	// State must be clean and restartable.
	s := streamSnapshotOf(t, url)
	if s.runLock {
		t.Error("run lock still held after stop")
	}
	if s.stop != nil {
		t.Error("stop channel not cleared after stop")
	}
	if s.signals != nil {
		t.Error("signals channel not cleared after stop")
	}
	if s.done != nil {
		t.Error("done channel not cleared after stop")
	}
	if s.clients != 0 {
		t.Errorf("viewers not cleared after stop: %d", s.clients)
	}
	if s.codecs != 0 {
		t.Error("codec information not cleared after stop")
	}
	if s.status != OFFLINE {
		t.Errorf("status not reset to offline: %d", s.status)
	}

	// A third stop on the already-stopped session is still a no-op.
	if err := Storage.StreamChannelStop(url); err != nil {
		t.Fatalf("repeated stop: %v", err)
	}
	if err := Storage.StreamChannelStop("rtsp://fake.invalid/never-existed"); err == nil {
		t.Fatal("expected an error stopping an unknown stream")
	}

	// Restart through the play entry point: a fresh collector and viewer.
	if res := ServerStreamWebRTC(url, testOffer); IsStreamError(res) {
		t.Fatalf("restart play failed: %q", res)
	}
	waitFor(t, "restart dial", func() bool { return dial.calls() >= 2 })
	waitFor(t, "restart viewer registered", func() bool { return liveClientCount(url) == 1 })
	// The fake source publishes codecs immediately, so the session goes online
	// without waiting for a keyframe.
	waitFor(t, "session online after restart", func() bool { return streamStatus(url) == ONLINE })

	// No goroutine leak: after stopping the restarted session, the goroutine
	// count must return to what it was before this test started the session.
	if err := Storage.StreamChannelStop(url); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	waitFor(t, "goroutines released", func() bool {
		return runtime.NumGoroutine() <= baseGoroutines+2
	})
}

// TestClientDeleteIsIdempotent checks that removing a viewer twice is safe and
// that the viewer is really gone from the session.
func TestClientDeleteIsIdempotent(t *testing.T) {
	shortenTimings(t)
	const url = "rtsp://fake.invalid/client-delete"

	src := newFakeSource(codecH264('a'))
	(&dialerFunc{fn: func(int) (streamSource, error) { return src, nil }}).install(t)
	cleanupStreams(t, url)

	if _, _, err := Storage.StreamChannelGetOrCreate(url, StreamST{URL: url}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	waitFor(t, "codecs available", func() bool { return len(streamCodecsOrNil(url)) > 0 })

	reg, err := Storage.ClientAddChecked(url, WEBRTC, 0)
	if err != nil {
		t.Fatalf("ClientAddChecked: %v", err)
	}
	if got := liveClientCount(url); got != 1 {
		t.Fatalf("expected one viewer, got %d", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Storage.ClientDelete(url, reg.id)
		}()
	}
	wg.Wait()

	if got := liveClientCount(url); got != 0 {
		t.Fatalf("viewer not removed: %d", got)
	}
	select {
	case <-reg.stop:
	default:
		t.Fatal("viewer stop channel was not closed on delete")
	}
	// Deleting an unknown viewer and an unknown stream must not panic.
	Storage.ClientDelete(url, "missing")
	Storage.ClientDelete("rtsp://fake.invalid/unknown-stream", reg.id)
}
