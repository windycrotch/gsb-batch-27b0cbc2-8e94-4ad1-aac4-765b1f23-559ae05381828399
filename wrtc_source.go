// WebRTC source connection.
// Receives data from a local websocket connection.
// Can send data to other local connections or to other nodes

package main

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

const rtcpPLIInterval = time.Second * 2

// WRTC_Source -This data structure contains the status data
// of a source connection (Client -> Node)
// The source registers itself into the node, and the node pipes
// the data to the sinks and external data senders
type WRTC_Source struct {
	requestId string // Unique Request ID in the websocket connection
	sid       string // Stream ID being pushed

	node       *WebRTC_CDN_Node    // Node reference
	connection *Connection_Handler // Websocket connection reference

	ready bool // If true, tracks are available

	closed bool // If true, source is no longer active

	peer sourcePeer // Peer transport (pion in production, fake in tests)

	statusMutex *sync.Mutex // Mutex to control access to the struct

	hasAudio        bool
	localTrackAudio mediaTrack // Audio track

	hasVideo        bool
	localTrackVideo mediaTrack // Video track
}

// Initialize
func (source *WRTC_Source) init() {
	source.closed = false
	source.ready = false
	source.statusMutex = &sync.Mutex{}
}

// Creates the transport and generates the offer
func (source *WRTC_Source) run() {
	source.statusMutex.Lock()
	defer source.statusMutex.Unlock()

	if source.closed {
		return
	}

	source.peer = source.node.newSourcePeer(source)
	source.peer.start()
}

// markReady is called by the peer transport when all the expected tracks have
// been received. It notifies the node so waiting sinks/senders are attached.
func (source *WRTC_Source) markReady(video mediaTrack, audio mediaTrack) {
	source.statusMutex.Lock()

	if source.closed || source.ready {
		source.statusMutex.Unlock()
		return
	}

	source.localTrackVideo = video
	source.localTrackAudio = audio
	source.ready = true

	source.statusMutex.Unlock()

	source.connection.logDebug("Source Ready | SreamID: " + source.sid + " | RequestID: " + source.requestId)

	// Notify the node without holding the source lock. The call is
	// synchronous: once it returns, every waiting sink has the new tracks,
	// which keeps the state transitions deterministic (and testable).
	source.node.onSourceReady(source)
}

// ICE Candidate message received from the client
func (source *WRTC_Source) onICECandidate(candidateJSON string) {
	source.statusMutex.Lock()
	defer source.statusMutex.Unlock()

	if source.closed || source.peer == nil || candidateJSON == "" {
		return
	}

	source.peer.onICECandidate(candidateJSON)
}

// ANSWER message received from the client
func (source *WRTC_Source) onAnswer(answerJSON string) {
	source.statusMutex.Lock()
	defer source.statusMutex.Unlock()

	if source.closed || source.peer == nil {
		return
	}

	source.peer.onAnswer(answerJSON)
}

// close tears the source down. It is idempotent: only the first call has any
// effect, so the transport disconnect path, an explicit CLOSE and the
// connection teardown path can all race safely.
//
// notifyConnection: send a CLOSE signaling message to the owning connection.
// deregister: remove the source from the node, releasing sinks back to STANDBY.
func (source *WRTC_Source) close(notifyConnection bool, deregister bool) {
	source.statusMutex.Lock()

	if source.closed {
		source.statusMutex.Unlock()
		return
	}

	source.closed = true
	source.ready = false

	peer := source.peer
	source.peer = nil

	source.statusMutex.Unlock()

	// Tear the transport down outside the lock.
	if peer != nil {
		peer.close()
	}

	// Notify the connection: release the request bookkeeping (identity
	// guarded) and send the CLOSE signaling message so the request-id may be
	// reused. Best effort: the socket may already be gone.
	if notifyConnection {
		source.connection.notifySourceClosed(source)
	}

	// Deregister from the node (identity-guarded against replacement).
	if deregister {
		source.node.onSourceClosed(source)
	}
}

// onPeerDisconnected is called by the peer transport when the underlying
// WebRTC connection closes or fails on its own.
func (source *WRTC_Source) onPeerDisconnected() {
	source.close(true, true)
}

// tracks returns the source's current local tracks. Used to invalidate the
// tracks of sinks when the source is closed or replaced.
func (source *WRTC_Source) tracks() (video mediaTrack, audio mediaTrack) {
	source.statusMutex.Lock()
	defer source.statusMutex.Unlock()

	return source.localTrackVideo, source.localTrackAudio
}

// pionSourcePeer is the production sourcePeer backed by a pion PeerConnection.
type pionSourcePeer struct {
	source         *WRTC_Source
	peerConnection *webrtc.PeerConnection
}

func newPionSourcePeer(source *WRTC_Source) sourcePeer {
	return &pionSourcePeer{source: source}
}

func (p *pionSourcePeer) start() {
	source := p.source

	peerConnectionConfig := loadWebRTCConfig() // Load config

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		LogError(err)
		return
	}

	p.peerConnection = peerConnection

	// Track event handler
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		source.statusMutex.Lock()
		defer source.statusMutex.Unlock()

		if source.closed {
			return
		}

		if remoteTrack.Kind() == webrtc.RTPCodecTypeVideo {
			// Received video track
			if source.localTrackVideo != nil {
				return
			}

			localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "video", "pion")
			if newTrackErr != nil {
				LogError(newTrackErr)
				return
			}

			source.localTrackVideo = localTrack

			go pipeTrack(remoteTrack, localTrack)

			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
			// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
			go func() {
				ticker := time.NewTicker(rtcpPLIInterval)
				for range ticker.C {
					if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())}}); rtcpSendErr != nil {
						if errors.Is(rtcpSendErr, io.ErrClosedPipe) {
							return
						}
					}
				}
			}()
		} else if remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
			// Received audio track
			if source.localTrackAudio != nil {
				return
			}

			localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "audio", "pion")
			if newTrackErr != nil {
				LogError(newTrackErr)
				return
			}

			source.localTrackAudio = localTrack

			go pipeTrack(remoteTrack, localTrack)
		} else {
			return
		}

		if (!source.hasAudio || source.localTrackAudio != nil) && (!source.hasVideo || source.localTrackVideo != nil) {
			// Received all the tracks
			source.markReady(source.localTrackVideo, source.localTrackAudio)
		}
	})

	// ICE Candidate handler
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		source.statusMutex.Lock()
		defer source.statusMutex.Unlock()

		if source.closed {
			return
		}

		if i != nil {
			b, e := json.Marshal(i.ToJSON())
			if e != nil {
				LogError(e)
			} else {
				source.connection.sendICECandidate(source.requestId, source.sid, string(b))
			}
		} else {
			source.connection.sendICECandidate(source.requestId, source.sid, "") // End of candidates
		}
	})

	// Connection status handler
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			source.connection.logDebug("Source Disconnected | SreamID: " + source.sid + " | RequestID: " + source.requestId)
			source.onPeerDisconnected() // Disconnected
		} else if state == webrtc.PeerConnectionStateConnected {
			source.connection.logDebug("Source Connected | SreamID: " + source.sid + " | RequestID: " + source.requestId)
		}
	})

	// Create transceivers

	if source.hasVideo {
		// Create transceiver to receive a VIDEO track
		if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			LogError(err)
			return
		}
	}

	if source.hasAudio {
		// Create transceiver to receive an AUDIO track
		if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
			LogError(err)
			return
		}
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

	// Send to the client

	offerJSON, e := json.Marshal(offer)

	if e != nil {
		LogError(e)
		return
	}

	source.connection.sendOffer(source.requestId, source.sid, string(offerJSON))
}

func (p *pionSourcePeer) onAnswer(answerJSON string) {
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
		return
	}
}

func (p *pionSourcePeer) onICECandidate(candidateJSON string) {
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

func (p *pionSourcePeer) close() {
	if p.peerConnection == nil {
		return
	}

	// Close the peer connection
	p.peerConnection.OnConnectionStateChange(nil)
	p.peerConnection.OnICECandidate(nil)
	p.peerConnection.OnTrack(nil)
	p.peerConnection.Close()

	p.peerConnection = nil
}
