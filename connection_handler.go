// Connection handler

package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Period to send HEARTBEAT messages to the client
const HEARTBEAT_MSG_PERIOD_SECONDS = 30

// Max time with no HEARTBEAT messages to consider the connection dead
const HEARTBEAT_TIMEOUT_MS = 2 * HEARTBEAT_MSG_PERIOD_SECONDS * 1000

// Request types
const REQUEST_TYPE_PUBLISH = 1
const REQUEST_TYPE_PLAY = 2

// Connection_Handler - Stores status data
// of an active connection
type Connection_Handler struct {
	id uint64 // Connection ID
	ip string // Client IP address

	port string // Client port (used to map test clients to their handler)

	node       *WebRTC_CDN_Node // Reference to the node
	connection *websocket.Conn  // Reference to the websocket connection

	lastHeartbeat int64 // Timestamp: Last time a HEARTBEAT message was received

	closed atomic.Bool // True if the connection is closed

	// ipCounted is true when this connection successfully took a per-IP
	// concurrency slot, so the slot is released exactly once on teardown.
	ipCounted bool

	sendingMutex *sync.Mutex // Mutex to control sending messages
	statusMutex  *sync.Mutex // Mutex to control access to the status data

	requests     map[string]int // List of requests
	requestCount uint32         // Request count

	sources map[string]*WRTC_Source // References to associated WebRTCs sources
	sinks   map[string]*WRTC_Sink   // References to associated WebRTC sinks
}

// Initialize
func (h *Connection_Handler) init() {
	h.sendingMutex = &sync.Mutex{}
	h.statusMutex = &sync.Mutex{}
	h.requestCount = 0
	h.requests = make(map[string]int)
	h.sources = make(map[string]*WRTC_Source)
	h.sinks = make(map[string]*WRTC_Sink)
}

// Runs the handler
// Reads messages, parses them and applies them
func (h *Connection_Handler) run() {
	defer func() {
		if err := recover(); err != nil {
			switch x := err.(type) {
			case string:
				h.log("Error: " + x)
			case error:
				h.log("Error: " + x.Error())
			default:
				h.log("Connection Crashed!")
			}
		}
		h.log("Connection closed.")
		// Ensure the socket is closed
		h.connection.Close()
		// Release all request resources (idempotent: safe even if individual
		// CLOSE messages already released some of them)
		h.onClose()
		// Release the per-IP quota slot and unregister the connection
		// (idempotent: a concurrent cleanup is a no-op)
		h.node.onConnectionClose(h.id, h.ip)
	}()

	c := h.connection

	h.log("Connection established.")

	h.lastHeartbeat = time.Now().UnixMilli()
	go h.sendHeartbeatMessages() // Start heartbeat

	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			break // Closed
		}

		if mt != websocket.TextMessage {
			continue
		}

		msg := parseSignalingMessage(string(message))
		h.logDebug("Received msg: " + msg.method)

		switch msg.method {
		case "HEARTBEAT":
			h.receiveHeartbeat()
		case "PUBLISH":
			h.receivePublishMessage(msg)
		case "PLAY":
			h.receivePlayMessage(msg)
		case "ANSWER":
			h.receiveAnswerMessage(msg)
		case "CANDIDATE":
			h.receiveCandidateMessage(msg)
		case "CLOSE":
			h.receiveCloseMessage(msg)
		default:
			h.logDebug("Unknown message: " + msg.method)
		}
	}
}

// reserveRequestLocked reserves a request-id on this socket.
// Must be called with h.statusMutex held.
// Returns an error code string, or "" on success. On failure no counter is
// touched, so rejected requests never leak or alter the quota.
func (h *Connection_Handler) reserveRequestLocked(requestId string, reqType int) string {
	if h.requests[requestId] != 0 {
		return "PROTOCOL_ERROR"
	}

	if h.requestCount >= h.node.requestLimit {
		return "LIMIT_REQUESTS"
	}

	h.requestCount++
	h.requests[requestId] = reqType

	return ""
}

// detachRequestLocked removes the bookkeeping of a request and returns the
// source/sink bound to it so the caller can close them outside the lock.
// Must be called with h.statusMutex held. Idempotent: an unknown request-id
// yields zero values and changes nothing, so requestCount can never go below
// zero and no other connection's quota can be affected.
func (h *Connection_Handler) detachRequestLocked(requestId string) (reqType int, source *WRTC_Source, sink *WRTC_Sink) {
	reqType = h.requests[requestId]
	if reqType == 0 {
		return 0, nil, nil
	}

	delete(h.requests, requestId)
	h.requestCount--

	if reqType == REQUEST_TYPE_PUBLISH {
		source = h.sources[requestId]
		delete(h.sources, requestId)
	} else if reqType == REQUEST_TYPE_PLAY {
		sink = h.sinks[requestId]
		delete(h.sinks, requestId)
	}

	return reqType, source, sink
}

// Called when a HEARTBEAT message is received from the client
func (h *Connection_Handler) receiveHeartbeat() {
	h.statusMutex.Lock()
	defer h.statusMutex.Unlock()

	h.lastHeartbeat = time.Now().UnixMilli()
}

// Checks if the client is sending HEARTBEAT messages
// If not, closes the connection
func (h *Connection_Handler) checkHeartbeat() {
	h.statusMutex.Lock()

	now := time.Now().UnixMilli()
	mustClose := (now - h.lastHeartbeat) >= HEARTBEAT_TIMEOUT_MS

	h.statusMutex.Unlock()

	if mustClose {
		h.connection.Close()
	}
}

// Task to send HEARTBEAT periodically
func (h *Connection_Handler) sendHeartbeatMessages() {
	for {
		time.Sleep(HEARTBEAT_MSG_PERIOD_SECONDS * time.Second)

		if h.closed.Load() {
			return // Closed
		}

		// Send heartbeat message
		msg := SignalingMessage{
			method: "HEARTBEAT",
			params: nil,
			body:   "",
		}
		h.send(msg)

		// Check heartbeat
		h.checkHeartbeat()
	}
}

// Called when a PUBLISH message is received from the client
func (h *Connection_Handler) receivePublishMessage(msg SignalingMessage) {
	requestId := msg.params["request-id"]
	streamId := msg.params["stream-id"]
	streamType := strings.ToUpper(msg.params["stream-type"])
	auth := msg.params["auth"]

	// Validate params

	if len(requestId) == 0 || len(requestId) > 255 {
		h.sendErrorMessage("INVALID_REQUEST_ID", "Request ID must be an string from 1 to 255 characters.", requestId)
		return
	}

	if len(streamId) == 0 || len(streamId) > 255 {
		h.sendErrorMessage("INVALID_STREAM_ID", "Stream ID must be an string from 1 to 255 characters.", requestId)
		return
	}

	if !checkAuthentication(auth, "stream_publish", streamId) {
		h.sendErrorMessage("INVALID_AUTH", "Invalid authentication provided.", requestId)
		return
	}

	hasAudio := true
	hasVideo := true

	if streamType == "AUDIO" {
		hasVideo = false
	} else if streamType == "VIDEO" {
		hasAudio = false
	}

	// Reserve the request-id before any resource is created. A rejection leaves
	// every counter untouched.
	h.statusMutex.Lock()
	errorCode := h.reserveRequestLocked(requestId, REQUEST_TYPE_PUBLISH)
	if errorCode != "" {
		h.statusMutex.Unlock()
		if errorCode == "LIMIT_REQUESTS" {
			h.sendErrorMessage(errorCode, "Too many requests on the same socket.", requestId)
		} else {
			h.sendErrorMessage(errorCode, "You reused the same request ID for 2 different requests.", requestId)
		}
		return
	}

	// Create source
	source := &WRTC_Source{
		requestId:  requestId,
		sid:        streamId,
		node:       h.node,
		hasAudio:   hasAudio,
		hasVideo:   hasVideo,
		connection: h,
	}

	source.init()

	h.sources[requestId] = source
	h.statusMutex.Unlock()

	h.sendOkMessage(requestId)

	// Registration (which may close/replace a previous source) and starting
	// the peer transport happen outside the handler lock to avoid holding two
	// locks at once.
	h.node.registerSource(source) // Register source

	go source.run() // Run source
}

// Called when a PLAY message is received from the client
func (h *Connection_Handler) receivePlayMessage(msg SignalingMessage) {
	requestId := msg.params["request-id"]
	streamId := msg.params["stream-id"]
	auth := msg.params["auth"]

	// Validate params

	if len(requestId) == 0 || len(requestId) > 255 {
		h.sendErrorMessage("INVALID_REQUEST_ID", "Request ID must be an string from 1 to 255 characters.", requestId)
		return
	}

	if len(streamId) == 0 || len(streamId) > 255 {
		h.sendErrorMessage("INVALID_STREAM_ID", "Stream ID must be an string from 1 to 255 characters.", requestId)
		return
	}

	if !checkAuthentication(auth, "stream_play", streamId) {
		h.sendErrorMessage("INVALID_AUTH", "Invalid authentication provided.", requestId)
		return
	}

	// Reserve the request-id before any resource is created.
	h.statusMutex.Lock()
	errorCode := h.reserveRequestLocked(requestId, REQUEST_TYPE_PLAY)
	if errorCode != "" {
		h.statusMutex.Unlock()
		if errorCode == "LIMIT_REQUESTS" {
			h.sendErrorMessage(errorCode, "Too many requests on the same socket.", requestId)
		} else {
			h.sendErrorMessage(errorCode, "You reused the same request ID for 2 different requests.", requestId)
		}
		return
	}

	sinkId := h.node.getSinkID()

	// Create sink
	sink := &WRTC_Sink{
		sinkId:     sinkId,
		requestId:  requestId,
		sid:        streamId,
		node:       h.node,
		connection: h,
	}

	sink.init()

	h.sinks[requestId] = sink
	h.statusMutex.Unlock()

	h.sendOkMessage(requestId)

	h.sendStandbyMessage(requestId)

	h.node.registerSink(sink) // Register sink
}

// Called when an ANSWER message is received from the client
func (h *Connection_Handler) receiveAnswerMessage(msg SignalingMessage) {
	requestId := msg.params["request-id"]

	h.statusMutex.Lock()
	reqType := h.requests[requestId]
	source := h.sources[requestId]
	sink := h.sinks[requestId]
	h.statusMutex.Unlock()

	if reqType == REQUEST_TYPE_PUBLISH && source != nil {
		source.onAnswer(msg.body)
	} else if reqType == REQUEST_TYPE_PLAY && sink != nil {
		sink.onAnswer(msg.body)
	}
}

// Called when a CANDIDATE message is received from the client
func (h *Connection_Handler) receiveCandidateMessage(msg SignalingMessage) {
	requestId := msg.params["request-id"]

	h.statusMutex.Lock()
	reqType := h.requests[requestId]
	source := h.sources[requestId]
	sink := h.sinks[requestId]
	h.statusMutex.Unlock()

	if reqType == REQUEST_TYPE_PUBLISH && source != nil {
		source.onICECandidate(msg.body)
	} else if reqType == REQUEST_TYPE_PLAY && sink != nil {
		sink.onICECandidate(msg.body)
	}
}

// Called when a CLOSE message is received from the client
func (h *Connection_Handler) receiveCloseMessage(msg SignalingMessage) {
	requestId := msg.params["request-id"]

	// Detach the bookkeeping under the lock, then close the resource outside.
	h.statusMutex.Lock()
	_, source, sink := h.detachRequestLocked(requestId)
	h.statusMutex.Unlock()

	if source != nil {
		// The client explicitly asked for the close: do not send another
		// CLOSE message back, but deregister from the node.
		source.close(false, true)
	} else if sink != nil {
		sink.close()
	}
}

// Sends a message to the client. Best effort: a write on a socket that is
// being torn down must never crash the goroutine reporting the event.
func (h *Connection_Handler) send(msg SignalingMessage) {
	defer func() {
		_ = recover()
	}()

	h.sendingMutex.Lock()
	defer h.sendingMutex.Unlock()

	h.connection.WriteMessage(websocket.TextMessage, []byte(msg.serialize()))
}

// Sends an ERROR message to the client
func (h *Connection_Handler) sendErrorMessage(code string, errMsg string, requestID string) {
	msg := SignalingMessage{
		method: "ERROR",
		params: make(map[string]string),
		body:   "",
	}

	msg.params["Error-Code"] = code
	msg.params["Error-Message"] = errMsg
	msg.params["Request-ID"] = requestID

	h.send(msg)
}

// Sends an OK message to the client
func (h *Connection_Handler) sendOkMessage(requestID string) {
	msg := SignalingMessage{
		method: "OK",
		params: make(map[string]string),
		body:   "",
	}

	msg.params["Request-ID"] = requestID

	h.send(msg)
}

// Sends a STANDBY message
func (h *Connection_Handler) sendStandbyMessage(requestID string) {
	msg := SignalingMessage{
		method: "STANDBY",
		params: make(map[string]string),
		body:   "",
	}

	msg.params["Request-ID"] = requestID

	h.send(msg)
}

// Sends an OFFER message to the client
func (h *Connection_Handler) sendOffer(reqId string, sid string, offerJSON string) {
	msg := SignalingMessage{
		method: "OFFER",
		params: make(map[string]string),
		body:   offerJSON,
	}

	msg.params["Request-ID"] = reqId
	msg.params["Stream-ID"] = sid

	h.send(msg)
}

// Sends a CANDIDATE message to the client
func (h *Connection_Handler) sendICECandidate(reqId string, sid string, candidateJSON string) {
	msg := SignalingMessage{
		method: "CANDIDATE",
		params: make(map[string]string),
		body:   candidateJSON,
	}

	msg.params["Request-ID"] = reqId
	msg.params["Stream-ID"] = sid

	h.send(msg)
}

// notifySourceClosed is called when a source closes on its own (transport
// failure, or because a new publisher replaced it). It releases the request
// bookkeeping, but only if the request is still bound to this source, and
// sends a CLOSE signaling message so the client may reuse the request-id.
// Identity checking makes it safe against the explicit-CLOSE and socket
// teardown paths racing with it.
func (h *Connection_Handler) notifySourceClosed(source *WRTC_Source) {
	h.statusMutex.Lock()

	if h.requests[source.requestId] == REQUEST_TYPE_PUBLISH && h.sources[source.requestId] == source {
		delete(h.sources, source.requestId)
		delete(h.requests, source.requestId)
		h.requestCount--
	}

	h.statusMutex.Unlock()

	msg := SignalingMessage{
		method: "CLOSE",
		params: make(map[string]string),
		body:   "",
	}

	msg.params["Request-ID"] = source.requestId
	msg.params["Stream-ID"] = source.sid

	h.send(msg)
}

// Logs a message for this connection
func (h *Connection_Handler) log(msg string) {
	LogRequest(h.id, h.ip, msg)
}

// Logs a debug message for this connection
func (h *Connection_Handler) logDebug(msg string) {
	LogDebugSession(h.id, h.ip, msg)
}

// onClose releases every request still bound to the connection. It is invoked
// when the socket goes away and is fully idempotent: an explicit CLOSE may
// already have released individual requests, and a concurrent/duplicate call
// finds nothing left to do. All resource teardown happens outside the lock.
func (h *Connection_Handler) onClose() {
	h.statusMutex.Lock()

	if h.closed.Load() {
		h.statusMutex.Unlock()
		return
	}

	h.closed.Store(true)

	type detachedResource struct {
		source *WRTC_Source
		sink   *WRTC_Sink
	}

	resources := make([]detachedResource, 0, len(h.requests))

	for requestId := range h.requests {
		_, source, sink := h.detachRequestLocked(requestId)
		resources = append(resources, detachedResource{source: source, sink: sink})
	}

	h.statusMutex.Unlock()

	for _, resource := range resources {
		if resource.source != nil {
			// The socket is gone: no CLOSE message, but still deregister.
			resource.source.close(false, true)
		}

		if resource.sink != nil {
			resource.sink.close()
		}
	}
}
