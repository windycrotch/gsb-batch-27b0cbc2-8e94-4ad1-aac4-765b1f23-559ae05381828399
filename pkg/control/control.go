package control

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"sync"
	"time"

	"github.com/Glimesh/waveguide/config"
	"github.com/Glimesh/waveguide/pkg/h264"
	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/orchestrator"
	"github.com/Glimesh/waveguide/pkg/service"
	"github.com/Glimesh/waveguide/pkg/types"
	"github.com/pion/rtp"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// streamIngest is the media ingest entrypoint used by StartStream. Tests can
// replace the per-Control field to avoid opening real WebRTC/HTTP connections.
func streamIngest(stream *Stream, ctx context.Context) error {
	return stream.Ingest(ctx)
}

// heartbeatInterval is how often the heartbeat goroutine collects metadata.
// It is a variable so tests can shorten it.
var heartbeatInterval = 15 * time.Second

// heartbeatStopTimeout bounds how long StopStream waits for an in-flight
// heartbeat goroutine to exit before cleaning up local state anyway.
var heartbeatStopTimeout = 5 * time.Second

type Pipe struct {
	Input        string
	Output       string
	Orchestrator string
}

type Control struct {
	ctx                context.Context
	service            service.Service
	orchestrator       orchestrator.Orchestrator
	streams            map[types.ChannelID]*Stream
	metadataCollectors map[types.ChannelID]chan struct{}

	// mu guards streams, metadataCollectors and channelOps.
	mu         sync.RWMutex
	channelOps map[types.ChannelID]*channelOp

	log     logrus.FieldLogger
	httpMux *http.ServeMux

	// ingest starts the media reader for a stream. It is a field so tests can
	// substitute a fake without touching package globals.
	ingest func(*Stream, context.Context) error

	Hostname       string
	HTTPServerType string `mapstructure:"http_server_type"`
	HTTPAddress    string `mapstructure:"http_address"`
	HTTPS          bool
	HTTPSHostname  string `mapstructure:"https_hostname"`
	HTTPSCert      string `mapstructure:"https_cert"`
	HTTPSKey       string `mapstructure:"https_key"`

	// Flag to enable saving video stream to file
	// Currently it's global flag toggled from the config file
	SaveVideo bool `mapstructure:"save_video"`
}

func New(
	ctx context.Context,
	cfg config.Config,
	hostname string,
	logger *logrus.Logger,
) (*Control, error) {
	svc := service.New(cfg, logger)
	if err := svc.Connect(); err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}

	or := orchestrator.New(cfg, hostname, logger)
	if err := or.Connect(); err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	httpCfg := cfg.Control

	return &Control{
		ctx:          ctx,
		service:      svc,
		orchestrator: or,

		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan struct{}),
		channelOps:         make(map[types.ChannelID]*channelOp),
		httpMux:            http.NewServeMux(),
		ingest:             streamIngest,
		log: logger.WithFields(logrus.Fields{
			"control": "waveguide",
		}),

		Hostname:       hostname,
		HTTPServerType: httpCfg.HTTPServerType,
		HTTPAddress:    httpCfg.Address,
		HTTPSHostname:  httpCfg.HTTPSHostname,
		HTTPSCert:      httpCfg.HTTPSCert,
		HTTPSKey:       httpCfg.HTTPSKey,

		// this should be controlled at a stream level
		SaveVideo: cfg.Control.SaveVideo,
	}, nil
}

func (ctrl *Control) Context() context.Context {
	return ctrl.ctx
}

func (ctrl *Control) ContextErr() error {
	return ctrl.Context().Err()
}

func (ctrl *Control) Shutdown() {
	ctrl.mu.RLock()
	channels := make([]types.ChannelID, 0, len(ctrl.metadataCollectors))
	for c := range ctrl.metadataCollectors {
		channels = append(channels, c)
	}
	ctrl.mu.RUnlock()

	for _, c := range channels {
		if err := ctrl.StopStream(c); err != nil {
			ctrl.log.Errorf("Failed to stop stream %s: %v", c, err)
		}
	}
}

// ErrStreamNotFound is returned when no live stream exists for a channel.
var ErrStreamNotFound = errors.New("stream does not exist in state")

// ErrStreamAlreadyExists is returned when a channel is started twice.
var ErrStreamAlreadyExists = errors.New("stream already exists in stream manager state")

// GetTracks returns a snapshot of the tracks for a live stream. It returns
// ErrStreamNotFound once the stream has stopped so viewers never receive
// tracks belonging to a session that is being torn down.
func (ctrl *Control) GetTracks(channelID types.ChannelID) ([]StreamTrack, error) {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return nil, err
	}

	return stream.LiveTracks()
}

func (ctrl *Control) GetHmacKey(channelID types.ChannelID) (string, error) {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return "", err
	}

	return string(actualKey), nil
}

func (ctrl *Control) Authenticate(channelID types.ChannelID, streamKey types.StreamKey) error {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return err
	}
	if string(streamKey) != string(actualKey) {
		return errors.New("incorrect stream key")
	}

	return nil
}

// channelOp serializes start/stop transitions for one channel. refs keeps the
// entry alive while any goroutine still holds it so the map can be pruned.
type channelOp struct {
	mu   sync.Mutex
	refs int
}

// lockChannel serializes start/stop transitions for a single channel without
// blocking operations on other channels. The returned function releases it.
func (ctrl *Control) lockChannel(channelID types.ChannelID) func() {
	ctrl.mu.Lock()
	op, ok := ctrl.channelOps[channelID]
	if !ok {
		op = &channelOp{}
		ctrl.channelOps[channelID] = op
	}
	op.refs++
	ctrl.mu.Unlock()

	op.mu.Lock()

	return func() {
		op.mu.Unlock()

		ctrl.mu.Lock()
		op.refs--
		if op.refs == 0 {
			delete(ctrl.channelOps, channelID)
		}
		ctrl.mu.Unlock()
	}
}

// StartStream starts a new stream session for channelID. Only one concurrent
// call per channel can succeed; the others receive ErrStreamAlreadyExists and
// leave the existing session untouched.
func (ctrl *Control) StartStream(channelID types.ChannelID) (*Stream, error) {
	unlock := ctrl.lockChannel(channelID)
	defer unlock()

	ctx, cancel := context.WithCancel(ctrl.Context())

	stream, err := ctrl.newStream(channelID, cancel)
	if err != nil {
		cancel()
		return nil, err
	}

	ctrl.log.Infof("Starting stream for %s", channelID)

	streamID, err := ctrl.service.StartStream(channelID)
	if err != nil {
		cancel()
		ctrl.removeStream(channelID, stream)
		return nil, err
	}
	stream.StreamID = streamID

	if err := ctrl.orchestrator.StartStream(stream.ChannelID, stream.StreamID); err != nil {
		// Tear the partially started session down locally as well as remotely,
		// otherwise the channel would be left registered but unusable.
		_ = ctrl.stopStream(stream)
		return nil, err
	}

	stream.setHeartbeatDone(make(chan struct{}))
	go ctrl.setupHeartbeat(stream)

	go func() {
		ingest := ctrl.ingest
		if ingest == nil {
			ingest = streamIngest
		}
		if err := ingest(stream, ctx); err != nil { //nolint not shadowed
			stream.log.Error(err)
			// Only stop if this exact session is still the live one. A new
			// session may have been started for the channel in the meantime,
			// and this stale goroutine must not tear it down.
			ctrl.stopStreamIfCurrent(stream)
		}
	}()

	return stream, nil
}

// StopStream stops the stream for a channel. The full stop sequence runs at
// most once per session; additional calls are treated as successful no-ops.
// Local state is always cleaned up, and any error from the service or
// orchestrator is returned to the caller after cleanup.
func (ctrl *Control) StopStream(channelID types.ChannelID) error {
	unlock := ctrl.lockChannel(channelID)
	defer unlock()

	stream, err := ctrl.getStream(channelID)
	if err != nil {
		// Already stopped or never started.
		return nil
	}

	return ctrl.stopStream(stream)
}

// stopStreamIfCurrent stops the stream only if it is still the live session
// registered for its channel. It protects against stale goroutines from a
// previous session (ingest failure, heartbeat failure) tearing down a session
// that was started after it.
func (ctrl *Control) stopStreamIfCurrent(stream *Stream) {
	unlock := ctrl.lockChannel(stream.ChannelID)
	defer unlock()

	current, err := ctrl.getStream(stream.ChannelID)
	if err != nil || current != stream {
		return
	}

	if err := ctrl.stopStream(stream); err != nil {
		stream.log.Error(err)
	}
}

// stopStream performs the actual teardown. Callers must hold the channel lock
// and must have resolved the stream from the live map, so it runs once per
// session even if several stop triggers race.
func (ctrl *Control) stopStream(stream *Stream) error {
	stream.Stop()

	// Wait until the heartbeat goroutine has observed the stop signal so no
	// metadata or thumbnail can be sent after this function returns. The wait
	// is bounded so a stuck external call cannot wedge the channel forever.
	stream.waitHeartbeat(heartbeatStopTimeout)

	// Always notify the remote sides, then always drop local state even if
	// they fail. Errors are reported after cleanup so the channel does not get
	// stuck in a half-stopped state.
	serviceErr := ctrl.service.EndStream(stream.StreamID)
	if serviceErr != nil {
		stream.log.Error(serviceErr)
	}
	orchestratorErr := ctrl.orchestrator.StopStream(stream.ChannelID, stream.StreamID)
	if orchestratorErr != nil {
		stream.log.Error(orchestratorErr)
	}

	ctrl.removeStream(stream.ChannelID, stream)

	if serviceErr != nil {
		return serviceErr
	}
	if orchestratorErr != nil {
		return orchestratorErr
	}
	return nil
}

var (
	ErrHeartbeatThumbnail             = errors.New("error sending thumbnail")
	ErrHeartbeatSendMetadata          = errors.New("error sending metadata")
	ErrHeartbeatOrchestratorHeartbeat = errors.New("error sending orchestrator heartbeat")
)

func (ctrl *Control) setupHeartbeat(stream *Stream) {
	done := stream.heartbeatDoneChan()
	if done != nil {
		defer close(done)
	}
	defer func() {
		if r := recover(); r != nil {
			stream.log.Errorf("heartbeat panicked: %v", r)
		}
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	tickFailed := 0

	for {
		// A stop signal wins over an already available tick: never send
		// metadata for a stream that has been asked to stop.
		select {
		case <-stream.stopHeartbeat:
			stream.log.Debug("heartbeat received stop signal")
			return
		default:
		}

		select {
		case <-stream.stopHeartbeat:
			stream.log.Debug("heartbeat received stop signal")
			return
		case <-ticker.C:
		}

		// Re-check the stop signal after the tick fired; select picks randomly
		// when both are ready, so this closes that window.
		select {
		case <-stream.stopHeartbeat:
			stream.log.Debug("heartbeat received stop signal")
			return
		default:
		}

		stream.log.Infof("Collecting metadata tickFailed=%d", tickFailed)
		hasErrors := false

		if err := ctrl.sendThumbnail(stream); err != nil {
			stream.log.Error(errors.Wrap(err, ErrHeartbeatThumbnail.Error()))
			hasErrors = true
		}

		if err := ctrl.sendMetadata(stream); err != nil {
			stream.log.Error(errors.Wrap(err, ErrHeartbeatSendMetadata.Error()))
			hasErrors = true
		}

		if err := ctrl.orchestrator.Heartbeat(stream.ChannelID); err != nil {
			stream.log.Error(errors.Wrap(err, ErrHeartbeatOrchestratorHeartbeat.Error()))
			hasErrors = true
		}

		if hasErrors {
			tickFailed++
		} else if tickFailed > 0 {
			tickFailed--
		}

		// Look for consecutive failures
		if tickFailed >= 5 {
			stream.log.Warn("Stopping stream due to excessive heartbeat errors")
			// Stop the whole channel, not just the local stream, and do it
			// off this goroutine because StopStream waits for it to exit.
			// Scoped to this session so it cannot kill a restarted one.
			go ctrl.stopStreamIfCurrent(stream)
			return
		}
	}
}

func (ctrl *Control) sendMetadata(stream *Stream) error {
	if stream.Stopped() {
		return nil
	}

	metadata := stream.metadataSnapshot()
	metadata.IngestServer = ctrl.Hostname

	return ctrl.service.UpdateStreamMetadata(stream.StreamID, metadata)
}

func (ctrl *Control) sendThumbnail(stream *Stream) (err error) {
	if stream.Stopped() {
		return nil
	}

	// signal the stream thumnailer to get me some thumbnails
	select {
	case stream.requestThumbnail <- struct{}{}:
	default:
	}

	ctrl.log.Debug("thumbnail requested")

	var data []byte
	// Since stream.lastThumbnail is a buffered chan, let's read all values to get the newest
	for len(stream.lastThumbnail) > 0 {
		data = <-stream.lastThumbnail
	}

	if len(data) == 0 {
		return nil
	}

	var img image.Image
	h264dec, err := h264.NewH264Decoder()
	if err != nil {
		return err
	}
	defer h264dec.Close()
	img, err = h264dec.Decode(data)
	if err != nil {
		return err
	}
	if img == nil {
		ctrl.log.WithField("channel_id", stream.ChannelID).Debug("img is nil")
		return nil
	}

	buff := new(bytes.Buffer)
	err = jpeg.Encode(buff, img, &jpeg.Options{
		Quality: 75,
	})
	if err != nil {
		return err
	}

	if err = ctrl.service.SendJpegPreviewImage(stream.StreamID, buff.Bytes()); err != nil {
		return err
	}

	ctrl.log.WithField("channel_id", stream.ChannelID).Debug("Got screenshot!")

	// Also update our metadata
	stream.setVideoSize(img.Bounds().Dx(), img.Bounds().Dy())

	return nil
}

// newStream registers a brand new session for the channel. It returns
// ErrStreamAlreadyExists if one is already registered, without touching it.
func (ctrl *Control) newStream(channelID types.ChannelID, cancelFunc context.CancelFunc) (*Stream, error) {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()

	if _, exists := ctrl.streams[channelID]; exists {
		return nil, ErrStreamAlreadyExists
	}

	stopHeartbeat := make(chan struct{})

	stream := &Stream{ //nolint exhaustive struct
		ChannelID: channelID,

		log:           ctrl.log.WithField("channel_id", channelID),
		whepURI:       ctrl.HTTPServerURL() + "/whep/endpoint/" + channelID.String(),
		authenticated: true,

		cancelFunc:        cancelFunc,
		kf:                keyframer.New(),
		rtpIngest:         make(chan *rtp.Packet),
		stopHeartbeat:     stopHeartbeat,
		stopThumbnailer:   make(chan struct{}, 1),
		thumbnailReceiver: make(chan *rtp.Packet, 50),
		requestThumbnail:  make(chan struct{}, 1),

		lastThumbnail: make(chan []byte, 1),

		startTime: time.Now().Unix(),
	}

	// TODO: this shouldn't be a global flag
	// but rather configured on per stream basis
	if ctrl.SaveVideo {
		stream.saveVideo = true
		stream.videoWriterChan = make(chan *rtp.Packet, 100) // not sure what the buffer size here should be
	}

	ctrl.streams[channelID] = stream
	ctrl.metadataCollectors[channelID] = stopHeartbeat

	return stream, nil
}

// removeStream deletes the channel state only if it still belongs to the given
// stream, so a stop of an old session can never evict a newly started one.
// It is idempotent.
func (ctrl *Control) removeStream(id types.ChannelID, stream *Stream) bool {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()

	current, exists := ctrl.streams[id]
	if !exists {
		return false
	}
	if stream != nil && current != stream {
		// A newer session owns the channel; leave it alone.
		return false
	}

	delete(ctrl.streams, id)
	delete(ctrl.metadataCollectors, id)
	return true
}

func (ctrl *Control) getStream(id types.ChannelID) (*Stream, error) {
	ctrl.mu.RLock()
	defer ctrl.mu.RUnlock()

	stream, exists := ctrl.streams[id]
	if !exists {
		return nil, ErrStreamNotFound
	}
	return stream, nil
}
