package control

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Glimesh/waveguide/config"
	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

// fakeService is an in-memory service.Service used to observe lifecycle calls.
type fakeService struct {
	mu sync.Mutex

	log            logrus.FieldLogger
	startErr       error
	endErr         error
	metadataErr    error
	nextStreamID   types.StreamID
	startCalls     int
	endCalls       int
	metadataCalls  int
	metadataByID   []types.StreamMetadata
	streamIDByCall []types.StreamID
	endCalledWith  []types.StreamID
	// blockMetadata, when non-nil, is closed to release UpdateStreamMetadata.
}

func (s *fakeService) SetLogger(log logrus.FieldLogger) { s.log = log }
func (s *fakeService) Name() string                     { return "fake" }
func (s *fakeService) Connect() error                   { return nil }

func (s *fakeService) GetHmacKey(channelID types.ChannelID) ([]byte, error) {
	return []byte(channelID.String()), nil
}

func (s *fakeService) StartStream(channelID types.ChannelID) (types.StreamID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.startCalls++
	if s.startErr != nil {
		return 0, s.startErr
	}
	if s.nextStreamID != 0 {
		id := s.nextStreamID
		s.nextStreamID++
		return id, nil
	}
	id := types.StreamID(channelID + 1)
	s.streamIDByCall = append(s.streamIDByCall, id)
	return id, nil
}

func (s *fakeService) EndStream(streamID types.StreamID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.endCalls++
	s.endCalledWith = append(s.endCalledWith, streamID)
	return s.endErr
}

func (s *fakeService) UpdateStreamMetadata(streamID types.StreamID, metadata types.StreamMetadata) error {
	s.mu.Lock()
	s.metadataCalls++
	s.metadataByID = append(s.metadataByID, metadata)
	err := s.metadataErr
	s.mu.Unlock()

	return err
}

func (s *fakeService) SendJpegPreviewImage(streamID types.StreamID, img []byte) error {
	return nil
}

func (s *fakeService) counts() (start, end, metadata int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startCalls, s.endCalls, s.metadataCalls
}

// fakeOrchestrator is an in-memory orchestrator.Orchestrator.
type fakeOrchestrator struct {
	mu sync.Mutex

	startErr     error
	stopErr      error
	heartbeatErr error
	startCalls   int
	stopCalls    int
	heartbeat    int
	stopChannels []types.ChannelID
	stopStreams  []types.StreamID
}

func (o *fakeOrchestrator) Name() string                     { return "fake" }
func (o *fakeOrchestrator) Connect() error                   { return nil }
func (o *fakeOrchestrator) Close() error                     { return nil }
func (o *fakeOrchestrator) SetLogger(log logrus.FieldLogger) {}

func (o *fakeOrchestrator) StartStream(channelID types.ChannelID, streamID types.StreamID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.startCalls++
	return o.startErr
}

func (o *fakeOrchestrator) StopStream(channelID types.ChannelID, streamID types.StreamID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopCalls++
	o.stopChannels = append(o.stopChannels, channelID)
	o.stopStreams = append(o.stopStreams, streamID)
	return o.stopErr
}

func (o *fakeOrchestrator) Heartbeat(channelID types.ChannelID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.heartbeat++
	return o.heartbeatErr
}

func (o *fakeOrchestrator) counts() (start, stop, heartbeat int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.startCalls, o.stopCalls, o.heartbeat
}

func testLogger() *logrus.Logger {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	return log
}

func newTestControl(t *testing.T) (*Control, *fakeService, *fakeOrchestrator) {
	t.Helper()

	ctrl, svc, or := newBareTestControl(t)

	// The default ingest path opens real WebRTC/HTTP connections; tests only
	// care about lifecycle bookkeeping, so use a fast heartbeat.
	prevInterval := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prevInterval })

	return ctrl, svc, or
}

func newBareTestControl(t *testing.T) (*Control, *fakeService, *fakeOrchestrator) {
	t.Helper()
	svc := &fakeService{nextStreamID: 0}
	or := &fakeOrchestrator{}

	ctrl := &Control{
		ctx:                context.Background(),
		service:            svc,
		orchestrator:       or,
		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan struct{}),
		channelOps:         make(map[types.ChannelID]*channelOp),
		httpMux:            nil,
		log:                testLogger().WithField("test", "control"),
	}
	ctrl.ingest = func(stream *Stream, ctx context.Context) error {
		<-ctx.Done()
		return nil
	}

	return ctrl, svc, or
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestStartStreamDuplicateIsRejected(t *testing.T) {
	ctrl, _, _ := newTestControl(t)

	const channel = types.ChannelID(100)

	first, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("first StartStream: %v", err)
	}

	second, err := ctrl.StartStream(channel)
	if !errors.Is(err, ErrStreamAlreadyExists) {
		t.Fatalf("second StartStream err = %v, want ErrStreamAlreadyExists", err)
	}
	if second != nil {
		t.Fatalf("second StartStream returned stream %v, want nil", second)
	}

	// The existing session must be intact and still resolvable.
	got, err := ctrl.getStream(channel)
	if err != nil {
		t.Fatalf("getStream after duplicate start: %v", err)
	}
	if got != first {
		t.Fatalf("duplicate start replaced existing stream")
	}
	if got.Stopped() {
		t.Fatalf("duplicate start stopped the existing stream")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
}

func TestStartStreamConcurrentOnlyOneWins(t *testing.T) {
	ctrl, _, _ := newTestControl(t)

	const channel = types.ChannelID(200)
	const goroutines = 32

	var successes int32
	var duplicates int32

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start

			_, err := ctrl.StartStream(channel)
			switch {
			case err == nil:
				atomic.AddInt32(&successes, 1)
			case errors.Is(err, ErrStreamAlreadyExists):
				atomic.AddInt32(&duplicates, 1)
			default:
				t.Errorf("unexpected StartStream error: %v", err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successful starts = %d, want 1", successes)
	}
	if duplicates != goroutines-1 {
		t.Fatalf("duplicate errors = %d, want %d", duplicates, goroutines-1)
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
}

func TestStopStreamIsIdempotent(t *testing.T) {
	ctrl, svc, or := newTestControl(t)

	const channel = types.ChannelID(300)

	if _, err := ctrl.StartStream(channel); err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)

	start := make(chan struct{})
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			errs <- ctrl.StopStream(channel)
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent StopStream returned error: %v", err)
		}
	}

	// Repeated stop after the fact is still a successful no-op.
	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("repeated StopStream: %v", err)
	}

	if _, end, _ := svc.counts(); end != 1 {
		t.Fatalf("EndStream calls = %d, want 1", end)
	}
	if _, stop, _ := or.counts(); stop != 1 {
		t.Fatalf("orchestrator StopStream calls = %d, want 1", stop)
	}

	if _, err := ctrl.getStream(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("channel still registered after stop: %v", err)
	}
}

func TestStopThenRestartUsesFreshSession(t *testing.T) {
	ctrl, svc, or := newTestControl(t)

	const channel = types.ChannelID(400)

	first, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("first StartStream: %v", err)
	}
	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("first StopStream: %v", err)
	}

	second, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("second StartStream: %v", err)
	}
	if second == first {
		t.Fatalf("restart reused the old Stream pointer")
	}
	if second.Stopped() {
		t.Fatalf("restarted stream is already marked stopped")
	}

	// The new session's heartbeat must be live and target the new stream.
	beforeMetadata := func() int {
		_, _, m := svc.counts()
		return m
	}()
	if !waitFor(t, 2*time.Second, func() bool { return beforeMetadata < func() int { _, _, m := svc.counts(); return m }() }) {
		t.Fatalf("restarted stream's heartbeat never ticked")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("second StopStream: %v", err)
	}

	_, end, _ := svc.counts()
	if end != 2 {
		t.Fatalf("EndStream calls = %d, want 2", end)
	}
	_, stop, _ := or.counts()
	if stop != 2 {
		t.Fatalf("orchestrator StopStream calls = %d, want 2", stop)
	}
}

func TestStopCleansLocalStateWhenRemoteFails(t *testing.T) {
	ctrl, svc, or := newBareTestControl(t)

	const channel = types.ChannelID(500)

	if _, err := ctrl.StartStream(channel); err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	serviceErr := errors.New("service down")
	orchestratorErr := errors.New("orchestrator down")
	svc.endErr = serviceErr
	or.stopErr = orchestratorErr

	err := ctrl.StopStream(channel)
	if !errors.Is(err, serviceErr) {
		t.Fatalf("StopStream err = %v, want service error", err)
	}

	// Local state must be gone regardless of the remote failure.
	if _, err := ctrl.getStream(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("channel still registered after failed stop: %v", err)
	}
	if _, err := ctrl.GetTracks(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("GetTracks after failed stop = %v, want ErrStreamNotFound", err)
	}

	// And the channel is immediately restartable.
	if _, err := ctrl.StartStream(channel); err != nil {
		t.Fatalf("restart after failed stop: %v", err)
	}

	// Clean up with working remote stubs.
	svc.endErr = nil
	or.stopErr = nil
	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("cleanup StopStream: %v", err)
	}
}

func TestStopReturnsOrchestratorErrorAfterCleanup(t *testing.T) {
	ctrl, _, or := newBareTestControl(t)

	const channel = types.ChannelID(550)

	if _, err := ctrl.StartStream(channel); err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	orchestratorErr := errors.New("orchestrator down")
	or.stopErr = orchestratorErr

	err := ctrl.StopStream(channel)
	if !errors.Is(err, orchestratorErr) {
		t.Fatalf("StopStream err = %v, want orchestrator error", err)
	}
	if _, err := ctrl.getStream(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("channel still registered after failed stop: %v", err)
	}
}

func TestStopSignalBeatsReadyHeartbeatTick(t *testing.T) {
	ctrl, svc, _ := newBareTestControl(t)

	const channel = types.ChannelID(600)

	prevInterval := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prevInterval })

	stream, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	// Let the heartbeat run a few times so the ticker is definitely active.
	if !waitFor(t, 2*time.Second, func() bool { _, _, m := svc.counts(); return m >= 1 }) {
		t.Fatalf("heartbeat never ran")
	}

	// Stop must make the heartbeat exit promptly...
	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
	select {
	case <-stream.heartbeatDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("heartbeat goroutine did not exit after stop")
	}

	// ...and no metadata may be sent after StopStream returned.
	_, _, before := svc.counts()
	time.Sleep(100 * time.Millisecond)
	_, _, after := svc.counts()
	if after != before {
		t.Fatalf("metadata sent after stop: before=%d after=%d", before, after)
	}
}

func TestHeartbeatStopDuringTickerRace(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(650)

	stream, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	// Fire the stop signal while the ticker is ready. Repeated to exercise the
	// random select choice in setupHeartbeat.
	for i := 0; i < 50; i++ {
		select {
		case stream.stopHeartbeat <- struct{}{}:
		default:
		}
		if stream.Stopped() {
			break
		}
	}
	stream.Stop()
	select {
	case <-stream.heartbeatDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("heartbeat did not exit after stop signal")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
}

func TestHeartbeatExcessiveFailuresStopsStream(t *testing.T) {
	ctrl, _, or := newBareTestControl(t)

	const channel = types.ChannelID(700)

	prevInterval := heartbeatInterval
	heartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prevInterval })

	or.heartbeatErr = errors.New("boom")

	if _, err := ctrl.StartStream(channel); err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		_, err := ctrl.getStream(channel)
		return errors.Is(err, ErrStreamNotFound)
	}) {
		t.Fatalf("channel was not stopped after excessive heartbeat failures")
	}

	// The failure path must go through the normal stop sequence exactly once.
	if _, stop, _ := or.counts(); stop != 1 {
		t.Fatalf("orchestrator StopStream calls = %d, want 1", stop)
	}

	// Further stops remain successful no-ops.
	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream after heartbeat failure: %v", err)
	}
}

func TestGetTracksUnknownChannelReturnsNotFound(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	if _, err := ctrl.GetTracks(types.ChannelID(800)); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("GetTracks unknown channel = %v, want ErrStreamNotFound", err)
	}
}

func TestGetTracksReturnsLiveTracksThenNotFound(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(801)

	stream, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if err := stream.AddTrack(videoTrack, webrtc.MimeTypeH264); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	tracks, err := ctrl.GetTracks(channel)
	if err != nil {
		t.Fatalf("GetTracks live channel: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Track != videoTrack {
		t.Fatalf("GetTracks returned %#v, want the live video track", tracks)
	}

	// Snapshot semantics: mutating the returned slice must not corrupt state.
	tracks[0] = StreamTrack{}
	if got, _ := ctrl.GetTracks(channel); got[0].Track != videoTrack {
		t.Fatalf("GetTracks did not return a snapshot")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}

	if _, err := ctrl.GetTracks(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("GetTracks after stop = %v, want ErrStreamNotFound", err)
	}
}

func TestGetTracksStoppedStreamReturnsNotFound(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(802)

	stream, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	// Mark the stream stopped without removing it from state, mimicking the
	// window between Stop() and removeStream during teardown.
	stream.Stop()

	if _, err := ctrl.GetTracks(channel); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("GetTracks on stopped stream = %v, want ErrStreamNotFound", err)
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
}

func TestGetTracksConcurrentWithStopNeverReturnsStaleTracks(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(803)

	stream, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if err := stream.AddTrack(videoTrack, webrtc.MimeTypeH264); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := ctrl.StopStream(channel); err != nil {
			t.Errorf("StopStream: %v", err)
		}
	}()

	for {
		tracks, err := ctrl.GetTracks(channel)
		if err != nil {
			if !errors.Is(err, ErrStreamNotFound) {
				t.Fatalf("GetTracks = %v, want nil or ErrStreamNotFound", err)
			}
			break
		}
		if len(tracks) != 1 {
			t.Fatalf("GetTracks returned %d tracks while live, want 1", len(tracks))
		}
		select {
		case <-done:
			// One more call must already report not found.
			if _, err := ctrl.GetTracks(channel); !errors.Is(err, ErrStreamNotFound) {
				t.Fatalf("GetTracks after stop = %v, want ErrStreamNotFound", err)
			}
			return
		default:
		}
	}
	<-done
}

func TestStaleIngestFailureDoesNotStopRestartedSession(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(850)

	// First session's ingest blocks until we release it, so we control the
	// ordering of its failure relative to the restart.
	failIngest := make(chan struct{})
	ctrl.ingest = func(stream *Stream, ctx context.Context) error {
		if stream.StreamID == 1 {
			<-failIngest
			return errors.New("stale ingest boom")
		}
		<-ctx.Done()
		return nil
	}

	first, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("first StartStream: %v", err)
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("StopStream: %v", err)
	}

	second, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("restart StartStream: %v", err)
	}
	if second == first {
		t.Fatalf("restart reused old stream")
	}

	// Now let the stale ingest goroutine fail; it must not touch the new one.
	close(failIngest)
	time.Sleep(100 * time.Millisecond)

	live, err := ctrl.getStream(channel)
	if err != nil {
		t.Fatalf("new session was removed by stale ingest failure: %v", err)
	}
	if live != second || live.Stopped() {
		t.Fatalf("new session was disrupted by stale ingest failure")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("cleanup StopStream: %v", err)
	}
}

func TestStopStreamIfCurrentIgnoresStaleStream(t *testing.T) {
	ctrl, svc, or := newBareTestControl(t)

	const channel = types.ChannelID(851)

	live, err := ctrl.StartStream(channel)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	// A stale pointer not registered for the channel must be a no-op.
	stale := &Stream{ChannelID: channel, log: testLogger()}
	ctrl.stopStreamIfCurrent(stale)

	if _, end, _ := svc.counts(); end != 0 {
		t.Fatalf("stale stop called EndStream %d times, want 0", end)
	}
	if _, stop, _ := or.counts(); stop != 0 {
		t.Fatalf("stale stop called orchestrator StopStream %d times, want 0", stop)
	}
	if got, err := ctrl.getStream(channel); err != nil || got != live {
		t.Fatalf("live session was disturbed by stale stop")
	}

	if err := ctrl.StopStream(channel); err != nil {
		t.Fatalf("cleanup StopStream: %v", err)
	}
}

func TestConcurrentStartStopKeepsStateConsistent(t *testing.T) {
	ctrl, _, _ := newBareTestControl(t)

	const channel = types.ChannelID(900)

	prevInterval := heartbeatInterval
	heartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prevInterval })

	const iterations = 40
	for i := 0; i < iterations; i++ {
		var wg sync.WaitGroup
		wg.Add(3)

		// Two starts can legitimately succeed in sequence if the racing stop
		// lands between them, so allow one slot per start.
		started := make(chan *Stream, 2)
		var unexpected error
		var unexpectedMu sync.Mutex

		go func() {
			defer wg.Done()
			s, err := ctrl.StartStream(channel)
			if err == nil {
				started <- s
				return
			}
			if !errors.Is(err, ErrStreamAlreadyExists) {
				unexpectedMu.Lock()
				unexpected = err
				unexpectedMu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			s, err := ctrl.StartStream(channel)
			if err == nil {
				started <- s
				return
			}
			if !errors.Is(err, ErrStreamAlreadyExists) {
				unexpectedMu.Lock()
				unexpected = err
				unexpectedMu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			// Racing stop before a session exists must not fail or panic.
			if err := ctrl.StopStream(channel); err != nil {
				unexpectedMu.Lock()
				unexpected = err
				unexpectedMu.Unlock()
			}
		}()

		wg.Wait()
		close(started)

		if unexpected != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, unexpected)
		}
		if n := len(started); n > 2 {
			t.Fatalf("iteration %d: %d successful starts, want at most 2", i, n)
		}

		if err := ctrl.StopStream(channel); err != nil {
			t.Fatalf("iteration %d: StopStream: %v", i, err)
		}

		// Whatever happened, the channel ends up cleanly stopped.
		if _, err := ctrl.GetTracks(channel); !errors.Is(err, ErrStreamNotFound) {
			t.Fatalf("iteration %d: GetTracks = %v, want ErrStreamNotFound", i, err)
		}
	}
}

func TestShutdownStopsAllStreams(t *testing.T) {
	ctrl, svc, or := newBareTestControl(t)

	channels := []types.ChannelID{1001, 1002, 1003}
	for _, c := range channels {
		if _, err := ctrl.StartStream(c); err != nil {
			t.Fatalf("StartStream(%d): %v", c, err)
		}
	}

	ctrl.Shutdown()

	for _, c := range channels {
		if _, err := ctrl.getStream(c); !errors.Is(err, ErrStreamNotFound) {
			t.Fatalf("channel %d still registered after Shutdown", c)
		}
	}
	if _, end, _ := svc.counts(); end != len(channels) {
		t.Fatalf("EndStream calls = %d, want %d", end, len(channels))
	}
	if _, stop, _ := or.counts(); stop != len(channels) {
		t.Fatalf("orchestrator StopStream calls = %d, want %d", stop, len(channels))
	}
}

func TestControlConstructorWiresMaps(t *testing.T) {
	// Guards against New() forgetting the new synchronization fields, which
	// would panic at runtime on the first start.
	cfg := config.Config{}
	cfg.Service.Type = "dummy"
	cfg.Orchestrator.Type = "dummy"

	ctrl, err := New(context.Background(), cfg, "test-host", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ctrl.channelOps == nil {
		t.Fatalf("New did not initialize channelOps")
	}
	if ctrl.streams == nil || ctrl.metadataCollectors == nil {
		t.Fatalf("New did not initialize stream maps")
	}
}
