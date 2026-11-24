package main

import (
	"strings"
	"time"

	"github.com/deepch/vdk/av"
	webrtc "github.com/deepch/vdk/format/webrtcv3"
	"github.com/sirupsen/logrus"
)

// streamErrorPrefix marks a play result that carries an error instead of an
// SDP answer. A real answer is standard base64, which never contains ':'
// followed by a space, so the marker is unambiguous.
const streamErrorPrefix = "error: "

// streamErrorMessage turns an error into a recognisable, non-empty play
// result. Callers must never report success for such a result.
func streamErrorMessage(err error) string {
	if err == nil {
		err = ErrorStreamChannelCodecNotFound
	}
	return streamErrorPrefix + err.Error()
}

// IsStreamError reports whether a play result is an error rather than an SDP
// answer. It is exported for callers (including tests and the frontend bridge)
// that must distinguish the two.
func IsStreamError(result string) bool {
	return result == "" || strings.HasPrefix(result, streamErrorPrefix)
}

// webrtcMuxer is the subset of the vdk WebRTC muxer the play path needs. It is
// an interface so tests can substitute a local fake and avoid opening real
// ICE/STUN sockets.
type webrtcMuxer interface {
	WritePacket(pkt av.Packet) error
	Close() error
}

// newWebRTCMuxer builds a muxer and writes the SDP answer. It is a package
// variable so tests can inject a fake.
var newWebRTCMuxer = func(codecs []av.CodecData, offer string) (webrtcMuxer, string, error) {
	m := webrtc.NewMuxer(webrtc.Options{
		ICEServers:    Storage.ServerICEServers(),
		ICEUsername:   Storage.ServerICEUsername(),
		ICECredential: Storage.ServerICECredential(),
		PortMin:       Storage.ServerWebRTCPortMin(),
		PortMax:       Storage.ServerWebRTCPortMax(),
	})
	answer, err := m.WriteHeader(codecs, offer)
	if err != nil {
		_ = m.Close()
		return nil, "", err
	}
	return m, answer, nil
}

// ServerStreamWebRTC is the play entry point. It guarantees:
//   - concurrent calls for the same stream identity share one collection
//     session (atomic get-or-create, never two collectors);
//   - failures are returned as recognisable, non-empty error strings, never as
//     an empty answer;
//   - the viewer is registered only after codec information is available, and
//     with the codec epoch it was built against, so a viewer never starts with
//     stale encoding parameters;
//   - registration failures are surfaced to the caller;
//   - an attached viewer survives an upstream reconnect and resumes on the
//     next keyframe.
func ServerStreamWebRTC(uuid string, data string) string {
	logger := log.WithFields(logrus.Fields{
		"module": "http_webrtc",
		"stream": uuid,
		"func":   "ServerStreamWebRTC",
	})

	if strings.TrimSpace(uuid) == "" {
		return streamErrorMessage(ErrorStreamEmptyURL)
	}
	if data == "" {
		return streamErrorMessage(ErrorStreamEmptyOffer)
	}

	// Atomic get-or-create: duplicate concurrent requests reuse the single
	// session instead of racing between an "exists" check and an "add".
	if _, _, err := Storage.StreamChannelGetOrCreate(uuid, StreamST{URL: uuid}); err != nil {
		logger.WithFields(logrus.Fields{"call": "StreamChannelGetOrCreate"}).Errorln(err)
		return streamErrorMessage(err)
	}

	// At most two attempts: if the codec set changes between capturing it and
	// registering, rebuild the muxer against the new codecs rather than
	// registering the viewer with outdated encoding parameters.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		codecs, epoch, err := Storage.StreamChannelCodecsEpoch(uuid)
		if err != nil {
			logger.WithFields(logrus.Fields{"call": "StreamChannelCodecs"}).Errorln(err)
			return streamErrorMessage(err)
		}
		muxer, answer, err := newWebRTCMuxer(codecs, data)
		if err != nil {
			logger.WithFields(logrus.Fields{"call": "WriteHeader"}).Errorln(err)
			return streamErrorMessage(err)
		}

		reg, err := Storage.ClientAddChecked(uuid, WEBRTC, epoch)
		if err != nil {
			_ = muxer.Close()
			if err == ErrorStreamChannelCodecsChanged {
				lastErr = err
				continue
			}
			logger.WithFields(logrus.Fields{"call": "ClientAdd"}).Errorln(err)
			return streamErrorMessage(err)
		}

		go serveWebRTCViewer(logger, uuid, reg, muxer)
		return answer
	}
	logger.WithFields(logrus.Fields{"call": "codec refresh"}).Errorln(lastErr)
	return streamErrorMessage(lastErr)
}

// serveWebRTCViewer forwards packets from the session to one WebRTC viewer.
// It unregisters the viewer when it leaves, times out, becomes too slow (the
// storage layer detaches it by closing reg.stop), or the muxer fails. The
// viewer survives an upstream reconnect: delta frames are withheld by the
// session until a fresh keyframe arrives, at which point the muxer resumes.
func serveWebRTCViewer(logger *logrus.Entry, streamID string, reg *clientRegistration, muxer webrtcMuxer) {
	defer func() {
		Storage.ClientDelete(streamID, reg.id)
		_ = muxer.Close()
	}()

	var videoStart bool
	viewerTimeout := reg.viewerTimeout
	if viewerTimeout <= 0 {
		viewerTimeout = streamViewerNoVideoTimeout
	}
	noVideo := time.NewTimer(viewerTimeout)
	defer noVideo.Stop()

	for {
		select {
		case <-reg.stop:
			// The session detached this viewer (leave, timeout or slow
			// consumer). Do not write further packets.
			return

		case <-noVideo.C:
			logger.WithFields(logrus.Fields{"call": "ErrorStreamNoVideo"}).Errorln(ErrorStreamNoVideo.Error())
			return

		case pck, ok := <-reg.avPackets:
			if !ok {
				return
			}
			if pck == nil {
				continue
			}
			if pck.IsKeyFrame {
				resetTimer(noVideo, viewerTimeout)
				videoStart = true
			}
			if !videoStart {
				continue
			}
			select {
			case <-reg.stop:
				return
			default:
			}
			if err := muxer.WritePacket(*pck); err != nil {
				logger.WithFields(logrus.Fields{"call": "WritePacket"}).Errorln(err)
				return
			}
		}
	}
}

// resetTimer resets t to fire after d, discarding a pending firing. It is safe
// for an already stopped timer.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
