// WebRTC sink connection.
// Sends data to a local websocket connection.

package main

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// WRTC_Sink - This data structure contains the status data
// of a sink connection (Node -> Client)
// The sink registers itself into the node, who notifies it
// when there are new tracks available
// The sink retries the connection is it's closed
// The sink will replace the existing connection with a new one if the source is replaced
type WRTC_Sink struct {
	sinkId    uint64 // Unique ID for the sink in the node
	requestId string // Unique request ID in the associated the websocket connection
	sid       string // Requested stream ID to pull

	node       *WebRTC_CDN_Node    // Reference to the node
	connection *Connection_Handler // Reference to the websocket connection

	closed bool // True when the sink is no longer active, prevent reconnection

	peer sinkPeer // Peer transport (pion in production, fake in tests)

	statusMutex *sync.Mutex // Mutex to control access to the struct

	hasAudio        bool
	localTrackAudio mediaTrack // Audio track

	hasVideo        bool
	localTrackVideo mediaTrack // Video track
}

// Initialize
func (sink *WRTC_Sink) init() {
	sink.statusMutex = &sync.Mutex{}
	sink.closed = false
}

// Receive the tracks from local source or relay
func (sink *WRTC_Sink) onTracksReady(localTrackVideo mediaTrack, localTrackAudio mediaTrack) {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.closed {
		return
	}

	// Set video track
	sink.localTrackVideo = localTrackVideo
	sink.hasVideo = localTrackVideo != nil

	// Set audio track
	sink.localTrackAudio = localTrackAudio
	sink.hasAudio = localTrackAudio != nil

	// If there is an existing connection, close it (source replaced / retry)
	if sink.peer != nil {
		sink.peer.close()
		sink.peer = nil
	}

	if !sink.hasVideo && !sink.hasAudio {
		return // Nothing to do
	}

	sink.peer = sink.node.newSinkPeer(sink)

	// Start the transport synchronously while holding only the sink lock.
	// Node-level callers invoke this after releasing the node lock, so there
	// is no nested locking and the OFFER is sent deterministically.
	sink.peer.start(sink.localTrackVideo, sink.localTrackAudio)
}

// startPeer asks the transport to create the peer connection and send the
// OFFER. It re-checks closure because it may run from a reconnect goroutine.
func (sink *WRTC_Sink) startPeer() {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.closed || sink.peer == nil || (!sink.hasVideo && !sink.hasAudio) {
		return
	}

	sink.peer.start(sink.localTrackVideo, sink.localTrackAudio)
}

func (sink *WRTC_Sink) onTracksClosed(localTrackVideo mediaTrack, localTrackAudio mediaTrack) {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.localTrackAudio == localTrackAudio && sink.localTrackVideo == localTrackVideo {
		sink.localTrackAudio = nil
		sink.localTrackVideo = nil
		sink.hasAudio = false
		sink.hasVideo = false

		if sink.peer != nil {
			sink.peer.close()
			sink.peer = nil
		}

		sink.connection.sendStandbyMessage(sink.requestId)
	}
}

// Call when an ICE Candidate message is received from the client via websocket
func (sink *WRTC_Sink) onICECandidate(candidateJSON string) {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.closed || sink.peer == nil || candidateJSON == "" {
		return
	}

	sink.peer.onICECandidate(candidateJSON)
}

// Call when the ANSWER is received from the client via websocket
func (sink *WRTC_Sink) onAnswer(answerJSON string) {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.closed || sink.peer == nil {
		return
	}

	sink.peer.onAnswer(answerJSON)
}

// Reconnect if the peer connection is closed, but the sink is still active
func (sink *WRTC_Sink) reconnect() {
	sink.statusMutex.Lock()
	defer sink.statusMutex.Unlock()

	if sink.peer != nil {
		sink.peer.close()
		sink.peer = nil
	}

	if sink.closed || (!sink.hasVideo && !sink.hasAudio) {
		return
	}

	sink.peer = sink.node.newSinkPeer(sink)
	go sink.startPeer()
}

// onPeerDisconnected is called by the peer transport when the underlying
// WebRTC connection closes or fails on its own.
func (sink *WRTC_Sink) onPeerDisconnected() {
	sink.reconnect()
}

// Close the sink. Idempotent: only the first call removes it from the node.
func (sink *WRTC_Sink) close() {
	sink.statusMutex.Lock()

	if sink.closed {
		sink.statusMutex.Unlock()
		return
	}

	sink.closed = true

	peer := sink.peer
	sink.peer = nil

	sink.hasAudio = false
	sink.hasVideo = false
	sink.localTrackAudio = nil
	sink.localTrackVideo = nil

	sink.statusMutex.Unlock()

	if peer != nil {
		peer.close()
	}

	sink.node.removeSink(sink)
}

// pionSinkPeer is the production sinkPeer backed by a pion PeerConnection.
type pionSinkPeer struct {
	sink           *WRTC_Sink
	peerConnection *webrtc.PeerConnection
	videoTrack     mediaTrack
	audioTrack     mediaTrack
}

func newPionSinkPeer(sink *WRTC_Sink) sinkPeer {
	return &pionSinkPeer{sink: sink}
}

func (p *pionSinkPeer) start(localTrackVideo mediaTrack, localTrackAudio mediaTrack) {
	sink := p.sink

	peerConnectionConfig := loadWebRTCConfig() // Load config

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		LogError(err)
		return
	}

	p.peerConnection = peerConnection

	// ICE candidate handler
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		sink.statusMutex.Lock()
		defer sink.statusMutex.Unlock()

		if sink.closed {
			return
		}

		if i != nil {
			b, e := json.Marshal(i.ToJSON())
			if e != nil {
				LogError(e)
			} else {
				sink.connection.sendICECandidate(sink.requestId, sink.sid, string(b))
			}
		} else {
			sink.connection.sendICECandidate(sink.requestId, sink.sid, "")
		}
	})

	// Connection status handler
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			sink.connection.logDebug("Sink Disconnected | sinkId: " + fmt.Sprint(sink.sinkId) + " | SreamID: " + sink.sid + " | RequestID: " + sink.requestId)
			sink.onPeerDisconnected() // If the connection fails, retry it
		} else if state == webrtc.PeerConnectionStateConnected {
			sink.connection.logDebug("Sink Connected | sinkId: " + fmt.Sprint(sink.sinkId) + " | SreamID: " + sink.sid + " | RequestID: " + sink.requestId)
		}
	})

	// Include the audio track
	if sink.hasAudio {
		audioTrack, ok := localTrackAudio.(*webrtc.TrackLocalStaticRTP)
		if !ok || audioTrack == nil {
			LogError(fmt.Errorf("invalid audio track type for sink %d", sink.sinkId))
			return
		}

		audioSender, err := peerConnection.AddTrack(audioTrack)
		if err != nil {
			LogError(err)
			return
		}

		go readPacketsFromRTPSender(audioSender)
	}

	// Include the video track
	if sink.hasVideo {
		videoTrack, ok := localTrackVideo.(*webrtc.TrackLocalStaticRTP)
		if !ok || videoTrack == nil {
			LogError(fmt.Errorf("invalid video track type for sink %d", sink.sinkId))
			return
		}

		videoSender, err := peerConnection.AddTrack(videoTrack)
		if err != nil {
			LogError(err)
			return
		}

		go readPacketsFromRTPSender(videoSender)
	}

	// Generate offer
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		LogError(err)
		return
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		LogError(err)
		return
	}

	// Send OFFER to the client

	offerJSON, e := json.Marshal(offer)

	if e != nil {
		LogError(e)
		return
	}

	sink.connection.sendOffer(sink.requestId, sink.sid, string(offerJSON))
}

func (p *pionSinkPeer) onAnswer(answerJSON string) {
	if p.peerConnection == nil {
		return
	}

	sd := webrtc.SessionDescription{}

	err := json.Unmarshal([]byte(answerJSON), &sd)

	if err != nil {
		LogError(err)
	}

	// Set the remote SessionDescription
	err = p.peerConnection.SetRemoteDescription(sd)

	if err != nil {
		LogError(err)
	}
}

func (p *pionSinkPeer) onICECandidate(candidateJSON string) {
	if p.peerConnection == nil {
		return
	}

	candidate := webrtc.ICECandidateInit{}

	err := json.Unmarshal([]byte(candidateJSON), &candidate)

	if err != nil {
		LogError(err)
	}

	err = p.peerConnection.AddICECandidate(candidate)

	if err != nil {
		LogError(err)
	}
}

func (p *pionSinkPeer) close() {
	if p.peerConnection == nil {
		return
	}

	p.peerConnection.OnICECandidate(nil)
	p.peerConnection.OnConnectionStateChange(nil)
	p.peerConnection.Close()

	p.peerConnection = nil
}
