package control

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

type StreamTrack struct {
	Type  webrtc.RTPCodecType
	Codec string
	Track webrtc.TrackLocal
}

type Stream struct {
	log logrus.FieldLogger

	cancelFunc context.CancelFunc
	stopped    bool

	// mu guards the mutable parts of the stream: the stopped flag, the tracks
	// and all metadata that the heartbeat goroutine reads while the ingest
	// goroutines are writing it.
	mu       sync.RWMutex
	stopOnce sync.Once

	// authenticated is set after the stream has successfully authed with a remote service
	authenticated bool

	whepURI string

	saveVideo   bool
	videoWriter FileWriter

	// mediaStarted is set after media bytes have come in from the client
	mediaStarted bool
	hasSomeAudio bool
	hasSomeVideo bool

	kf            *keyframer.Keyframer
	rtpIngest     chan *rtp.Packet
	lastThumbnail chan []byte
	// channel used to signal thumbnailer to stop
	stopThumbnailer   chan struct{}
	stopHeartbeat     chan struct{}
	requestThumbnail  chan struct{}
	thumbnailReceiver chan *rtp.Packet

	// heartbeatDone is closed once the stream's heartbeat goroutine exits.
	// It is only used to make tests deterministic.
	heartbeatDone chan struct{}

	videoWriterChan chan *rtp.Packet

	ChannelID types.ChannelID
	StreamID  types.StreamID
	StreamKey types.StreamKey

	tracks []StreamTrack

	// Raw Metadata
	startTime           int64
	lastTime            int64 // Last time the metadata collector ran
	audioBps            int
	videoBps            int
	totalAudioPackets   int
	totalVideoPackets   int
	lastAudioPackets    int
	lastVideoPackets    int
	clientVendorName    string
	clientVendorVersion string
	videoCodec          string
	audioCodec          string
	videoHeight         int
	videoWidth          int
}

func (s *Stream) AddTrack(track webrtc.TrackLocal, codec string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: Needs better support for tracks with different codecs
	if track.Kind() == webrtc.RTPCodecTypeAudio {
		s.hasSomeAudio = true
		s.audioCodec = codec
	} else if track.Kind() == webrtc.RTPCodecTypeVideo {
		s.hasSomeVideo = true
		s.videoCodec = codec
	} else {
		return errors.New("unexpected track kind")
	}

	s.tracks = append(s.tracks, StreamTrack{
		Type:  track.Kind(),
		Track: track,
		Codec: codec,
	})

	return nil
}

// Tracks returns a copy of the tracks that have been added to the stream, so
// callers can iterate over them without racing with the ingest path.
func (s *Stream) Tracks() []StreamTrack {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.tracksLocked()
}

func (s *Stream) tracksLocked() []StreamTrack {
	tracks := make([]StreamTrack, len(s.tracks))
	copy(tracks, s.tracks)
	return tracks
}

// LiveTracks returns a snapshot of the stream's tracks, or ErrStreamNotFound
// if the stream has already been stopped. The check and the copy happen under
// one lock so callers never receive tracks for a session that is stopping.
func (s *Stream) LiveTracks() ([]StreamTrack, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.stopped {
		return nil, ErrStreamNotFound
	}
	return s.tracksLocked(), nil
}

func (s *Stream) ReportMetadata(metadatas ...Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, metadata := range metadatas {
		metadata(s)
	}

	return nil
}

// Stop marks the stream as stopped and tears down its local resources. It is
// safe to call concurrently and any number of times; only the first call has
// an effect.
func (s *Stream) Stop() {
	s.stopOnce.Do(func() {
		if s.log != nil {
			s.log.Infof("stopping stream")
		}

		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()

		if s.stopHeartbeat != nil {
			close(s.stopHeartbeat)
		}
		if s.cancelFunc != nil {
			s.cancelFunc()
		}

		if s.log != nil {
			s.log.Debug("canceled stream ctx")
		}
	})
}

func (s *Stream) Stopped() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.stopped
}

func (s *Stream) setVideoSize(width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.videoWidth = width
	s.videoHeight = height
}

// setHeartbeatDone publishes the channel the heartbeat goroutine closes on
// exit. It must be set before the goroutine starts.
func (s *Stream) setHeartbeatDone(done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.heartbeatDone = done
}

func (s *Stream) heartbeatDoneChan() chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.heartbeatDone
}

// waitHeartbeat blocks until the stream's heartbeat goroutine has exited, or
// until timeout elapses. It is a no-op when no heartbeat was ever started.
func (s *Stream) waitHeartbeat(timeout time.Duration) {
	done := s.heartbeatDoneChan()
	if done == nil {
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		if s.log != nil {
			s.log.Warn("timed out waiting for heartbeat to exit")
		}
	}
}

// metadataSnapshot returns a consistent copy of the stream metadata and
// updates the last metadata collection time.
func (s *Stream) metadataSnapshot() types.StreamMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastTime = time.Now().Unix()

	return types.StreamMetadata{
		AudioCodec:        s.audioCodec,
		IngestViewers:     0,
		LostPackets:       0, // Don't exist
		NackPackets:       0, // Don't exist
		RecvPackets:       s.totalAudioPackets + s.totalVideoPackets,
		SourceBitrate:     0, // Likely just need to calculate the bytes between two 5s snapshots?
		SourcePing:        0, // Not accessible unless we ping them manually
		StreamTimeSeconds: int(s.lastTime - s.startTime),
		VendorName:        s.clientVendorName,
		VendorVersion:     s.clientVendorVersion,
		VideoCodec:        s.videoCodec,
		VideoHeight:       s.videoHeight,
		VideoWidth:        s.videoWidth,
	}
}
