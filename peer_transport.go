// Peer transport abstraction.
//
// The signaling/state machine of sources and sinks is decoupled from the
// concrete WebRTC implementation (pion) behind the sourcePeer and sinkPeer
// interfaces. Production code uses the pion-based factories; tests can inject
// in-memory fake peers (see peer_transport_fake_test.go) so the real signaling
// entry point can be driven end-to-end without any external WebRTC peer.

package main

import "github.com/pion/webrtc/v4"

// mediaTrack is the subset of *webrtc.TrackLocalStaticRTP the node logic
// depends on. Pion tracks satisfy it directly; tests provide fake tracks whose
// identity can be asserted.
type mediaTrack interface {
	ID() string
	StreamID() string
}

// Compile-time assertion: the concrete pion track satisfies mediaTrack.
var _ mediaTrack = (*webrtc.TrackLocalStaticRTP)(nil)

// sourcePeer abstracts the transport backing a WRTC_Source.
type sourcePeer interface {
	// start creates the peer connection, sets up the event handlers and
	// sends the initial OFFER to the client.
	start()
	// onAnswer delivers a remote SDP answer.
	onAnswer(answerJSON string)
	// onICECandidate delivers a remote ICE candidate.
	onICECandidate(candidateJSON string)
	// close tears the peer connection down. It must be idempotent.
	close()
}

// sinkPeer abstracts the transport backing a WRTC_Sink.
type sinkPeer interface {
	// start creates the peer connection bound to the given local tracks and
	// sends the initial OFFER to the client.
	start(video mediaTrack, audio mediaTrack)
	// onAnswer delivers a remote SDP answer.
	onAnswer(answerJSON string)
	// onICECandidate delivers a remote ICE candidate.
	onICECandidate(candidateJSON string)
	// close tears the peer connection down. It must be idempotent.
	close()
}

// Factories used to create peer transports. They are fields on the node so
// tests may replace them with in-memory implementations.
type sourcePeerFactory func(source *WRTC_Source) sourcePeer
type sinkPeerFactory func(sink *WRTC_Sink) sinkPeer
