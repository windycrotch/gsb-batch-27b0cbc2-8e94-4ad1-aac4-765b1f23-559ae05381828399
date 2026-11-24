package main

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/deepch/vdk/av"
	"github.com/sirupsen/logrus"
)

var debug = true

// Storage is the process wide session registry behind the play entry point.
// Stream values are kept as pointers so that the session runtime state (run
// lock, signals channel, viewers, codecs) is never copied by accident.
var Storage = StorageST{
	Streams: make(map[string]*StreamST),
}

// Default stream  type
const (
	MSE = iota
	WEBRTC
	RTSP
)

// Default stream status type
const (
	OFFLINE = iota
	ONLINE
)

// Default stream errors
var (
	Success                         = "success"
	ErrorStreamNotFound             = errors.New("stream not found")
	ErrorStreamAlreadyExists        = errors.New("stream already exists")
	ErrorStreamChannelAlreadyExists = errors.New("stream channel already exists")
	ErrorStreamNotHLSSegments       = errors.New("stream hls not ts seq found")
	ErrorStreamNoVideo              = errors.New("stream no video")
	ErrorStreamNoClients            = errors.New("stream no clients")
	ErrorStreamRestart              = errors.New("stream restart")
	ErrorStreamStopCoreSignal       = errors.New("stream stop core signal")
	ErrorStreamStopRTSPSignal       = errors.New("stream stop rtsp signal")
	ErrorStreamChannelNotFound      = errors.New("stream channel not found")
	ErrorStreamChannelCodecNotFound = errors.New("stream channel codec not ready, possible stream offline")
	ErrorStreamsLen0                = errors.New("streams len zero")
	ErrorStreamUnauthorized         = errors.New("stream request unauthorized")
	// ErrorStreamEmptyURL is returned when a play request carries no usable
	// stream identity.
	ErrorStreamEmptyURL = errors.New("stream url is empty")
	// ErrorStreamEmptyOffer is returned when a play request carries no SDP
	// offer to answer.
	ErrorStreamEmptyOffer = errors.New("stream offer is empty")
	// ErrorStreamChannelCodecsChanged is returned when the encoding
	// information changed between the moment a viewer was prepared and the
	// moment it tried to register. It is retryable: the caller must rebuild
	// its decoder against the new codecs.
	ErrorStreamChannelCodecsChanged = errors.New("stream channel codecs changed")
	// ErrorStreamChannelStopped is returned when a viewer tries to register on
	// a session that is not collecting anymore.
	ErrorStreamChannelStopped = errors.New("stream channel stopped")
)

// StorageST main storage struct
type StorageST struct {
	mutex   sync.RWMutex
	Server  ServerST             `json:"server" groups:"api,config"`
	Streams map[string]*StreamST `json:"streams,omitempty" groups:"api,config"`
}

// ServerST server storage section
type ServerST struct {
	LogLevel      logrus.Level `json:"log_level" groups:"api,config"`
	ICEServers    []string     `json:"ice_servers" groups:"api,config"`
	ICEUsername   string       `json:"ice_username" groups:"api,config"`
	ICECredential string       `json:"ice_credential" groups:"api,config"`
	WebRTCPortMin uint16       `json:"webrtc_port_min" groups:"api,config"`
	WebRTCPortMax uint16       `json:"webrtc_port_max" groups:"api,config"`
}

// StreamST stream storage section. The exported fields are configuration; the
// unexported ones are runtime state guarded by StorageST.mutex.
type StreamST struct {
	Name               string `json:"name,omitempty" groups:"api,config"`
	URL                string `json:"url,omitempty" groups:"api,config"`
	OnDemand           bool   `json:"on_demand,omitempty" groups:"api,config"`
	Debug              bool   `json:"debug,omitempty" groups:"api,config"`
	Status             int    `json:"status,omitempty" groups:"api"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty" groups:"api,config"`
	Audio              bool   `json:"audio,omitempty" groups:"api,config"`

	// runLock is true while a collection goroutine owns the session.
	runLock bool
	// generation identifies the current collection goroutine. Every (re)start
	// bumps it so that late writes from an obsolete goroutine are dropped.
	generation uint64
	// codecEpoch is bumped whenever the published encoding information
	// changes (new connection, codec update, connection loss). A viewer that
	// captured an older epoch must not register against the newer codecs.
	codecEpoch uint64
	codecs     []av.CodecData
	sdp        []byte
	// signals carries control signals to the collection goroutine.
	signals chan int
	// stop is closed to ask the collection goroutine to shut down. It is nil
	// when no goroutine is running.
	stop chan struct{}
	// done is closed by the collection goroutine when it exits.
	done    chan struct{}
	clients map[string]*ClientST
	ack     time.Time
}

// ClientST client storage section
type ClientST struct {
	mode              int
	outgoingAVPacket  chan *av.Packet
	outgoingRTPPacket chan *[]byte
	socket            net.Conn
	// stop is closed when the session detaches the viewer. Consumers select on
	// it so a detached (for example too slow) viewer stops promptly.
	stop chan struct{}
	// stopped guards the stop channel against double close.
	stopped bool
	// needKeyframe is true while the viewer has not yet been sent a keyframe
	// of the current connection. Delta frames are withheld until then.
	needKeyframe bool
	// viewerTimeout is how long the viewer waits for a keyframe before giving
	// up. It is captured at registration so a viewer goroutine never reads
	// mutable process wide timing state.
	viewerTimeout time.Duration
	// queueLimit is the maximum number of buffered packets for this viewer.
	queueLimit int
}
