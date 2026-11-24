package main

import (
	"time"

	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/format/rtspv2"
	"github.com/sirupsen/logrus"
)

// Collector timing. Key ones are variables so tests can shorten them; package
// variables are only mutated by tests that run in isolation.
var (
	// streamReconnectDelay is the pause between two upstream connection
	// attempts of the same session.
	streamReconnectDelay = 2 * time.Second
	// streamKeyTestTimeout is how long a connection may stay silent (no
	// keyframe) before it is considered dead and retried.
	streamKeyTestTimeout = 20 * time.Second
	// streamCheckClientsInterval is how often an on-demand session verifies
	// that it still has viewers.
	streamCheckClientsInterval = 20 * time.Second
)

// streamSource abstracts the upstream feed a collection session consumes. The
// real implementation wraps deepch/vdk's RTSP client; tests inject a local
// fake, so no real RTSP server or network access is needed.
type streamSource interface {
	// Codecs returns the codec information known at connection time. It may be
	// empty when the codecs arrive later via SignalCodecUpdate.
	Codecs() []av.CodecData
	// SDP returns the raw session description at connection time.
	SDP() []byte
	// WaitCodec reports whether codecs must wait for SignalCodecUpdate.
	WaitCodec() bool
	// Packets delivers demuxed media packets.
	Packets() <-chan *av.Packet
	// RTP delivers raw RTP packets for proxy viewers.
	RTP() <-chan *[]byte
	// Signals delivers source side control signals (codec update, stop).
	Signals() <-chan int
	// Close releases the connection.
	Close()
}

// streamSourceDialer opens an upstream source. It is a variable so tests can
// substitute a local fake.
var streamSourceDialer = func(opt StreamST) (streamSource, error) {
	client, err := rtspv2.Dial(rtspv2.RTSPClientOptions{
		URL:                opt.URL,
		InsecureSkipVerify: opt.InsecureSkipVerify,
		DisableAudio:       !opt.Audio,
		DialTimeout:        3 * time.Second,
		ReadWriteTimeout:   5 * time.Second,
		Debug:              opt.Debug,
		OutgoingProxy:      true,
	})
	if err != nil {
		return nil, err
	}
	return &rtspStreamSource{client: client}, nil
}

// rtspStreamSource adapts deepch/vdk's RTSP client to streamSource.
type rtspStreamSource struct {
	client *rtspv2.RTSPClient
}

func (s *rtspStreamSource) Codecs() []av.CodecData     { return s.client.CodecData }
func (s *rtspStreamSource) SDP() []byte                { return s.client.SDPRaw }
func (s *rtspStreamSource) WaitCodec() bool            { return s.client.WaitCodec }
func (s *rtspStreamSource) Packets() <-chan *av.Packet { return s.client.OutgoingPacketQueue }
func (s *rtspStreamSource) RTP() <-chan *[]byte        { return s.client.OutgoingProxyQueue }
func (s *rtspStreamSource) Signals() <-chan int        { return s.client.Signals }
func (s *rtspStreamSource) Close()                     { s.client.Close() }

// StreamServerRunStreamDo starts the collection session of streamID if it is
// not already running. It exists for callers that only know the stream id;
// normal session creation goes through StreamChannelAdd/GetOrCreate, which
// start the collector atomically with the session.
func StreamServerRunStreamDo(streamID string) {
	Storage.mutex.Lock()
	defer Storage.mutex.Unlock()
	s, ok := Storage.Streams[streamID]
	if !ok {
		return
	}
	Storage.startCollectorLocked(streamID, s)
}

// runCollector owns one generation of a collection session. It reconnects the
// upstream source until it is told to stop, the session is stopped, or an
// on-demand session runs out of viewers.
func (obj *StorageST) runCollector(streamID string, generation uint64, signals chan int, stop chan struct{}, done chan struct{}, opt StreamST) {
	defer close(done)
	defer obj.finalizeCollector(streamID, generation)

	logger := log.WithFields(logrus.Fields{
		"module": "core",
		"stream": streamID,
		"func":   "runCollector",
	})
	for {
		select {
		case <-stop:
			return
		default:
		}

		if opt.OnDemand && !obj.ClientHas(streamID) {
			logger.WithFields(logrus.Fields{"call": "ClientHas"}).Infoln("Stop stream no client")
			return
		}

		status, err := obj.StreamServerRunStream(streamID, generation, signals, stop, opt)
		if err != nil {
			logger.WithFields(logrus.Fields{"call": "StreamServerRunStream"}).Infoln("Stream exit", err)
		}
		// A positive status means "do not reconnect": the session was stopped,
		// the stream lost its last on-demand viewer, or the source asked to
		// stop for good.
		if status > 0 {
			return
		}

		timer := time.NewTimer(streamReconnectDelay)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// StreamServerRunStream runs one upstream connection: it publishes the codec
// information, then forwards packets to the session viewers until the
// connection drops or the session is stopped. It returns a positive status
// when the session must not reconnect.
func (obj *StorageST) StreamServerRunStream(streamID string, generation uint64, signals <-chan int, stop <-chan struct{}, opt StreamST) (int, error) {
	logger := log.WithFields(logrus.Fields{
		"module": "core",
		"stream": streamID,
		"func":   "StreamServerRunStream",
	})

	src, err := streamSourceDialer(opt)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	// The session may have been stopped (or restarted) while dialing.
	if !obj.streamChannelCurrent(streamID, generation) {
		return 1, ErrorStreamChannelStopped
	}

	waitCodec := src.WaitCodec()
	if !waitCodec {
		obj.streamChannelConnectionUp(streamID, generation, src.Codecs(), src.SDP())
	} else {
		// Keep the session online but with no codec information yet; viewers
		// wait for the codec update signal.
		obj.streamChannelConnectionUp(streamID, generation, nil, nil)
	}

	logger.WithFields(logrus.Fields{"call": "Start"}).Infoln("Success connection to source")

	// When this connection ends the encoding information is invalidated. The
	// viewers stay attached and will resynchronise on the next keyframe.
	defer obj.streamChannelConnectionDown(streamID, generation)

	keyTest := time.NewTimer(streamKeyTestTimeout)
	defer keyTest.Stop()
	checkClients := time.NewTimer(streamCheckClientsInterval)
	defer checkClients.Stop()

	var start bool
	var fps int
	var probeCount int
	var probeFrame int
	var probePTS time.Duration

	for {
		select {
		case <-stop:
			return 2, ErrorStreamStopCoreSignal

		case <-checkClients.C:
			if opt.OnDemand && !obj.ClientHas(streamID) {
				return 1, ErrorStreamNoClients
			}
			checkClients.Reset(streamCheckClientsInterval)

		case <-keyTest.C:
			return 0, ErrorStreamNoVideo

		case signal := <-signals:
			switch signal {
			case SignalStreamStop:
				return 2, ErrorStreamStopCoreSignal
			case SignalStreamRestart:
				return 0, ErrorStreamRestart
			case SignalStreamClient:
				return 1, ErrorStreamNoClients
			}

		case signal := <-src.Signals():
			switch signal {
			case rtspv2.SignalCodecUpdate:
				if obj.streamChannelPublishCodecs(streamID, generation, src.Codecs(), src.SDP()) {
					waitCodec = false
				}
			case rtspv2.SignalStreamRTPStop:
				return 0, ErrorStreamStopRTSPSignal
			}

		case packetRTP, ok := <-src.RTP():
			if !ok {
				return 0, ErrorStreamStopRTSPSignal
			}
			obj.streamChannelCastProxyFor(streamID, generation, packetRTP)

		case packetAV, ok := <-src.Packets():
			if !ok {
				return 0, ErrorStreamStopRTSPSignal
			}
			if waitCodec || packetAV == nil {
				continue
			}
			if packetAV.IsKeyFrame {
				keyTest.Reset(streamKeyTestTimeout)
			}
			if packetAV.IsKeyFrame && !start {
				start = true
			}
			// FPS probe: normalise packet durations to a stable frame rate.
			if start {
				probePTS += packetAV.Duration
				probeFrame++
				if packetAV.IsKeyFrame && probePTS.Seconds() >= 1 {
					probeCount++
					if probeCount == 2 && probePTS > 0 {
						fps = int(float64(probeFrame)/probePTS.Seconds() + 0.5)
					}
					probeFrame = 0
					probePTS = 0
				}
			}
			if start && fps > 0 {
				// A 1000/fps millisecond frame is time.Second/fps nanoseconds.
				packetAV.Duration = time.Second / time.Duration(fps)
			}
			obj.streamChannelCastFor(streamID, generation, packetAV)
		}
	}
}
