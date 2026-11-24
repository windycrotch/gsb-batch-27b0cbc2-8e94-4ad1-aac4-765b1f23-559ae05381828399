package main

import (
	"time"

	"github.com/deepch/vdk/av"
)

// streamClientAckTimeout is how long a session without activity is considered
// alive for on-demand collection purposes.
const streamClientAckTimeout = 30 * time.Second

// clientRegistration is the handle a viewer gets from ClientAddChecked. The
// viewer owns the registration until ClientDelete is called; ClientDelete is
// idempotent.
type clientRegistration struct {
	id string
	// avPackets receives video/audio packets (WEBRTC viewers).
	avPackets chan *av.Packet
	// rtpPackets receives raw RTP packets (RTSP proxy viewers).
	rtpPackets chan *[]byte
	// stop is closed when the session detaches the viewer (leave, timeout,
	// slow consumer or stream stop).
	stop chan struct{}
	// viewerTimeout is the keyframe wait time captured at registration.
	viewerTimeout time.Duration
}

// ClientAdd registers a viewer against the current codec information. It is
// kept for callers that do not track the codec epoch; prefer ClientAddChecked.
func (obj *StorageST) ClientAdd(streamID string, mode int) (string, chan *av.Packet, chan *[]byte, error) {
	reg, err := obj.ClientAddChecked(streamID, mode, 0)
	if err != nil {
		return "", nil, nil, err
	}
	return reg.id, reg.avPackets, reg.rtpPackets, nil
}

// ClientAddChecked registers a viewer against the session, verifying that the
// viewer's encoding information is still current. A non-zero codecEpoch must
// match the session's epoch; when it does not, the registration is rejected
// with ErrorStreamChannelCodecsChanged so the caller can rebuild against the
// new codecs instead of starting a viewer with stale parameters.
//
// Registration only succeeds while the session is collecting. Errors are
// always returned to the caller; nothing is swallowed.
func (obj *StorageST) ClientAddChecked(streamID string, mode int, codecEpoch uint64) (*clientRegistration, error) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	streamTmp, ok := obj.Streams[streamID]
	if !ok {
		return nil, ErrorStreamNotFound
	}
	if !streamTmp.runLock {
		return nil, ErrorStreamChannelStopped
	}
	if codecEpoch != 0 && streamTmp.codecEpoch != codecEpoch {
		return nil, ErrorStreamChannelCodecsChanged
	}
	if streamTmp.clients == nil {
		streamTmp.clients = make(map[string]*ClientST)
	}
	cid, err := generateUUID()
	if err != nil {
		return nil, err
	}
	chAV := make(chan *av.Packet, streamClientQueueLimit)
	chRTP := make(chan *[]byte, streamClientQueueLimit)
	client := &ClientST{
		mode:              mode,
		outgoingAVPacket:  chAV,
		outgoingRTPPacket: chRTP,
		stop:              make(chan struct{}),
		needKeyframe:      mode != RTSP,
		queueLimit:        streamClientQueueLimit,
		viewerTimeout:     streamViewerNoVideoTimeout,
	}
	streamTmp.clients[cid] = client
	streamTmp.ack = time.Now()
	return &clientRegistration{
		id:            cid,
		avPackets:     chAV,
		rtpPackets:    chRTP,
		stop:          client.stop,
		viewerTimeout: client.viewerTimeout,
	}, nil
}

// ClientDelete removes a viewer from the session and releases its stop
// channel. It is idempotent: deleting an unknown viewer or deleting twice is a
// harmless no-op and never panics.
func (obj *StorageST) ClientDelete(streamID string, cid string) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	streamTmp, ok := obj.Streams[streamID]
	if !ok {
		return
	}
	client, ok := streamTmp.clients[cid]
	if !ok {
		return
	}
	obj.detachClientLocked(streamTmp, cid, client)
}

// ClientCount returns how many viewers are currently attached to a session.
func (obj *StorageST) ClientCount(streamID string) int {
	obj.mutex.RLock()
	defer obj.mutex.RUnlock()
	streamTmp, ok := obj.Streams[streamID]
	if !ok {
		return 0
	}
	return len(streamTmp.clients)
}

// ClientHas reports whether the session has recent activity, which keeps an
// on-demand collection session alive.
func (obj *StorageST) ClientHas(streamID string) bool {
	obj.mutex.RLock()
	defer obj.mutex.RUnlock()
	streamTmp, ok := obj.Streams[streamID]
	if !ok {
		return false
	}
	if time.Since(streamTmp.ack) > streamClientAckTimeout {
		return false
	}
	return true
}
