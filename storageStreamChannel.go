package main

import (
	"strings"
	"time"

	"github.com/deepch/vdk/av"
)

// Default timing and capacity constants of a stream session.
const (
	// streamCodecWaitTimeout bounds how long a viewer waits for codec info.
	streamCodecWaitTimeout = 5 * time.Second
	// streamCodecPollInterval is the polling interval used while waiting for
	// codec info to become available.
	streamCodecPollInterval = 10 * time.Millisecond
	// streamClientQueueLimit is the maximum number of packets buffered for a
	// single viewer. A viewer exceeding it is detached, never allowed to block
	// the collector or the other viewers.
	streamClientQueueLimit = 500
	// streamStopJoinTimeout bounds how long Stop waits for the collector to
	// exit. The session is left restartable even if the join times out.
	streamStopJoinTimeout = 2 * time.Second
)

// streamViewerNoVideoTimeout is how long a viewer waits for the first keyframe
// (or the next keyframe after a reconnect) before giving up. It is a variable
// so tests can shorten it.
var streamViewerNoVideoTimeout = 10 * time.Second

// StreamChannelMake prepares a StreamST value for storage. Its runtime state
// (viewers, ack, signals, codecs) is always created fresh so a restarted
// session never inherits stale state from a previous run.
func (obj *StorageST) StreamChannelMake(val StreamST) StreamST {
	val.clients = make(map[string]*ClientST)
	val.ack = time.Now()
	val.signals = make(chan int, 100)
	val.codecs = nil
	val.sdp = nil
	val.codecEpoch = 1
	val.Status = OFFLINE
	val.runLock = false
	val.stop = nil
	val.done = nil
	return val
}

// StreamChannelGetOrCreate is the play entry point. For a given stream
// identity it atomically reuses the existing session or creates a new one, and
// guarantees that at most one collection goroutine owns the session. The
// "created" result reports whether this call inserted the session.
//
// The returned pointer is owned by Storage. Callers must use the accessors
// rather than reading mutable runtime fields directly.
func (obj *StorageST) StreamChannelGetOrCreate(streamID string, val StreamST) (*StreamST, bool, error) {
	if strings.TrimSpace(streamID) == "" {
		return nil, false, ErrorStreamEmptyURL
	}
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if s, ok := obj.Streams[streamID]; ok {
		s.ack = time.Now()
		obj.startCollectorLocked(streamID, s)
		return s, false, nil
	}
	tmp := obj.StreamChannelMake(val)
	if strings.TrimSpace(tmp.URL) == "" {
		tmp.URL = streamID
	}
	obj.Streams[streamID] = &tmp
	obj.startCollectorLocked(streamID, &tmp)
	return &tmp, true, nil
}

// StreamChannelAdd creates a new session. It is safe for concurrent use: a
// duplicate request fails with ErrorStreamChannelAlreadyExists and never
// starts a second collector. Callers that want reuse semantics should use
// StreamChannelGetOrCreate.
func (obj *StorageST) StreamChannelAdd(uuid string, val StreamST) error {
	if strings.TrimSpace(uuid) == "" {
		return ErrorStreamEmptyURL
	}
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if _, ok := obj.Streams[uuid]; ok {
		return ErrorStreamChannelAlreadyExists
	}
	tmp := obj.StreamChannelMake(val)
	obj.Streams[uuid] = &tmp
	obj.startCollectorLocked(uuid, &tmp)
	return nil
}

// startCollectorLocked starts the single collection goroutine for a session.
// obj.mutex must be held. Starting is idempotent while a collector is alive.
func (obj *StorageST) startCollectorLocked(streamID string, s *StreamST) {
	if s.runLock {
		return
	}
	// Defensive: a session restarted after its collector exited must not
	// inherit viewer entries whose goroutines would otherwise leak.
	obj.detachAllClientsLocked(s)
	s.generation++
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.signals = make(chan int, 100)
	s.clients = make(map[string]*ClientST)
	s.codecs = nil
	s.sdp = nil
	s.codecEpoch++
	s.Status = OFFLINE
	// Give a freshly started session a grace period so the viewer that
	// triggered the start can register before the no-client check runs.
	s.ack = time.Now()
	s.runLock = true
	opt := StreamST{
		URL:                s.URL,
		OnDemand:           s.OnDemand,
		Audio:              s.Audio,
		InsecureSkipVerify: s.InsecureSkipVerify,
		Debug:              s.Debug,
	}
	go obj.runCollector(streamID, s.generation, s.signals, s.stop, s.done, opt)
}

// StreamChannelStop stops the collection session and returns it to a clean,
// restartable state. It is idempotent: repeated or concurrent calls do not
// panic, and viewers belonging to the session are detached exactly once.
func (obj *StorageST) StreamChannelStop(streamID string) error {
	obj.mutex.Lock()
	s, ok := obj.Streams[streamID]
	if !ok {
		obj.mutex.Unlock()
		return ErrorStreamNotFound
	}
	stopCh := s.stop
	doneCh := s.done
	running := s.runLock

	// Take ownership of the stop channel so a second Stop cannot close it
	// again. The session entry itself is kept (and cleaned) so a later Play
	// can restart it.
	s.stop = nil
	s.done = nil
	s.generation++
	newGeneration := s.generation
	s.runLock = false
	s.signals = nil
	s.codecs = nil
	s.sdp = nil
	s.codecEpoch++
	s.Status = OFFLINE
	s.ack = time.Time{}
	obj.detachAllClientsLocked(s)
	obj.mutex.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	if running && doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(streamStopJoinTimeout):
		}
	}
	// The collector exits after this generation is invalidated, so its own
	// finalizeCollector call is a no-op. Clear the leftover done channel here
	// so a stopped session reports a fully clean state.
	obj.mutex.Lock()
	if s2, ok := obj.Streams[streamID]; ok && !s2.runLock && s2.generation == newGeneration {
		s2.done = nil
	}
	obj.mutex.Unlock()
	return nil
}

// finalizeCollector clears the session state when its collector exits. It is a
// no-op if a newer generation has already taken over.
func (obj *StorageST) finalizeCollector(streamID string, generation uint64) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	s, ok := obj.Streams[streamID]
	if !ok || s.generation != generation {
		return
	}
	s.runLock = false
	s.stop = nil
	s.done = nil
	s.signals = nil
	s.codecs = nil
	s.sdp = nil
	s.codecEpoch++
	s.Status = OFFLINE
	s.ack = time.Time{}
	obj.detachAllClientsLocked(s)
}

// streamChannelCurrent reports whether generation still owns the session.
func (obj *StorageST) streamChannelCurrent(streamID string, generation uint64) bool {
	obj.mutex.RLock()
	defer obj.mutex.RUnlock()
	s, ok := obj.Streams[streamID]
	return ok && s.runLock && s.generation == generation
}

// StreamChannelUnlock clears the run lock if the session has no live
// collector. Kept for backwards compatibility; normal shutdown flows through
// StreamChannelStop and finalizeCollector.
func (obj *StorageST) StreamChannelUnlock(streamID string) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if streamTmp, ok := obj.Streams[streamID]; ok {
		streamTmp.runLock = false
	}
}

// StreamChannelControl returns the stored stream pointer.
func (obj *StorageST) StreamChannelControl(key string) (*StreamST, error) {
	obj.mutex.RLock()
	defer obj.mutex.RUnlock()
	if streamTmp, ok := obj.Streams[key]; ok {
		return streamTmp, nil
	}
	return nil, ErrorStreamNotFound
}

// StreamChannelExist checks that a stream exists and refreshes its activity
// timestamp.
func (obj *StorageST) StreamChannelExist(streamID string) bool {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if streamTmp, ok := obj.Streams[streamID]; ok {
		streamTmp.ack = time.Now()
		return true
	}
	return false
}

// StreamChannelReload asks the current collector to reconnect.
func (obj *StorageST) StreamChannelReload(uuid string) error {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if tmp, ok := obj.Streams[uuid]; ok {
		if tmp.signals == nil {
			return ErrorStreamChannelNotFound
		}
		select {
		case tmp.signals <- SignalStreamRestart:
		default:
		}
		return nil
	}
	return ErrorStreamNotFound
}

// StreamChannelInfo returns the stored stream pointer.
func (obj *StorageST) StreamChannelInfo(uuid string) (*StreamST, error) {
	obj.mutex.RLock()
	defer obj.mutex.RUnlock()
	if tmp, ok := obj.Streams[uuid]; ok {
		return tmp, nil
	}
	return nil, ErrorStreamNotFound
}

// StreamChannelCodecs returns a copy of the codec information of the current
// connection, waiting up to streamCodecWaitTimeout for it to become available.
// Codec info is cleared as soon as a connection drops, so a stopped or
// reconnecting session reports an error instead of handing out stale data.
func (obj *StorageST) StreamChannelCodecs(streamID string) ([]av.CodecData, error) {
	codecs, _, err := obj.StreamChannelCodecsEpoch(streamID)
	return codecs, err
}

// StreamChannelCodecsEpoch is StreamChannelCodecs plus the codec epoch the
// returned information belongs to. A viewer captures the epoch and passes it
// to ClientAddChecked so registration is rejected if the encoding information
// changed in between.
func (obj *StorageST) StreamChannelCodecsEpoch(streamID string) ([]av.CodecData, uint64, error) {
	deadline := time.Now().Add(streamCodecWaitTimeout)
	for {
		obj.mutex.RLock()
		tmp, ok := obj.Streams[streamID]
		if !ok {
			obj.mutex.RUnlock()
			return nil, 0, ErrorStreamNotFound
		}
		if len(tmp.codecs) > 0 {
			ret := append([]av.CodecData(nil), tmp.codecs...)
			epoch := tmp.codecEpoch
			obj.mutex.RUnlock()
			return ret, epoch, nil
		}
		running := tmp.runLock
		obj.mutex.RUnlock()

		if !running {
			return nil, 0, ErrorStreamChannelStopped
		}
		if !time.Now().Before(deadline) {
			return nil, 0, ErrorStreamChannelCodecNotFound
		}
		time.Sleep(streamCodecPollInterval)
	}
}

// StreamChannelStatus changes stream status unconditionally.
func (obj *StorageST) StreamChannelStatus(key string, val int) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if tmp, ok := obj.Streams[key]; ok {
		tmp.Status = val
	}
}

// streamChannelSetStatus changes status only if generation still owns the
// session, so a shutting-down collector cannot overwrite a newer session.
func (obj *StorageST) streamChannelSetStatus(key string, generation uint64, val int) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if tmp, ok := obj.Streams[key]; ok && tmp.generation == generation {
		tmp.Status = val
	}
}

// StreamChannelCast broadcasts a video packet unconditionally.
func (obj *StorageST) StreamChannelCast(key string, val *av.Packet) {
	obj.streamChannelCast(key, 0, val)
}

// streamChannelCastFor broadcasts a video packet on behalf of a specific
// collector generation. Packets from an obsolete collector are dropped.
func (obj *StorageST) streamChannelCastFor(key string, generation uint64, val *av.Packet) {
	obj.streamChannelCast(key, generation, val)
}

// streamChannelCast broadcasts a video packet for the given collector
// generation (0 means the current generation). Viewers that have not yet seen
// a keyframe of the current connection do not receive delta frames, and
// viewers whose queue is backed up are detached instead of blocking.
func (obj *StorageST) streamChannelCast(key string, generation uint64, val *av.Packet) {
	if val == nil {
		return
	}
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	tmp, ok := obj.Streams[key]
	if !ok || (generation != 0 && tmp.generation != generation) {
		return
	}
	if len(tmp.clients) == 0 {
		return
	}
	for cid, c := range tmp.clients {
		if c.mode == RTSP || c.stopped {
			continue
		}
		if c.needKeyframe {
			if !val.IsKeyFrame {
				continue
			}
			c.needKeyframe = false
		}
		limit := c.queueLimit
		if limit <= 0 {
			limit = streamClientQueueLimit
		}
		if len(c.outgoingAVPacket) >= limit {
			obj.detachClientLocked(tmp, cid, c)
			continue
		}
		select {
		case c.outgoingAVPacket <- val:
		default:
			obj.detachClientLocked(tmp, cid, c)
		}
	}
	tmp.ack = time.Now()
}

// StreamChannelCastProxy broadcasts an RTP packet unconditionally.
func (obj *StorageST) StreamChannelCastProxy(key string, val *[]byte) {
	obj.streamChannelCastProxy(key, 0, val)
}

// streamChannelCastProxyFor is StreamChannelCastProxy for one generation.
func (obj *StorageST) streamChannelCastProxyFor(key string, generation uint64, val *[]byte) {
	obj.streamChannelCastProxy(key, generation, val)
}

// streamChannelCastProxy is the RTSP-client counterpart of streamChannelCast.
func (obj *StorageST) streamChannelCastProxy(key string, generation uint64, val *[]byte) {
	if val == nil {
		return
	}
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	tmp, ok := obj.Streams[key]
	if !ok || (generation != 0 && tmp.generation != generation) {
		return
	}
	if len(tmp.clients) == 0 {
		return
	}
	for cid, c := range tmp.clients {
		if c.mode != RTSP || c.stopped {
			continue
		}
		limit := c.queueLimit
		if limit <= 0 {
			limit = streamClientQueueLimit
		}
		if len(c.outgoingRTPPacket) >= limit {
			obj.detachClientLocked(tmp, cid, c)
			continue
		}
		select {
		case c.outgoingRTPPacket <- val:
		default:
			obj.detachClientLocked(tmp, cid, c)
		}
	}
	tmp.ack = time.Now()
}

// StreamChannelCodecsUpdate updates codec storage unconditionally.
func (obj *StorageST) StreamChannelCodecsUpdate(streamID string, val []av.CodecData, sdp []byte) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if tmp, ok := obj.Streams[streamID]; ok {
		tmp.codecs = val
		tmp.sdp = sdp
		tmp.codecEpoch++
	}
}

// streamChannelPublishCodecs stores codecs for a connection belonging to the
// given collector generation. It reports whether the write was applied.
func (obj *StorageST) streamChannelPublishCodecs(streamID string, generation uint64, val []av.CodecData, sdp []byte) bool {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	tmp, ok := obj.Streams[streamID]
	if !ok || tmp.generation != generation {
		return false
	}
	tmp.codecs = val
	tmp.sdp = sdp
	tmp.codecEpoch++
	return true
}

// streamChannelConnectionUp marks the session online, publishes the encoding
// information of the new connection and tells existing viewers to wait for a
// fresh keyframe (their previous decoding state may not continue across a
// source reconnect). Viewers stay attached across reconnects.
func (obj *StorageST) streamChannelConnectionUp(streamID string, generation uint64, val []av.CodecData, sdp []byte) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	tmp, ok := obj.Streams[streamID]
	if !ok || tmp.generation != generation {
		return
	}
	tmp.Status = ONLINE
	if len(val) > 0 {
		tmp.codecs = append([]av.CodecData(nil), val...)
		tmp.sdp = append([]byte(nil), sdp...)
		tmp.codecEpoch++
	}
	tmp.ack = time.Now()
	for _, c := range tmp.clients {
		if c.mode != RTSP {
			c.needKeyframe = true
		}
	}
}

// streamChannelConnectionDown invalidates the encoding information of a dropped
// connection while keeping viewers attached, so they can resume once the
// collector reconnects. The epoch is bumped so a viewer that captured the old
// codecs can never register against a newer connection.
func (obj *StorageST) streamChannelConnectionDown(streamID string, generation uint64) {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	tmp, ok := obj.Streams[streamID]
	if !ok || tmp.generation != generation {
		return
	}
	tmp.Status = OFFLINE
	tmp.codecs = nil
	tmp.sdp = nil
	tmp.codecEpoch++
	for _, c := range tmp.clients {
		if c.mode != RTSP {
			c.needKeyframe = true
		}
	}
}

// StreamChannelSDP returns the SDP of the current connection, waiting up to
// streamCodecWaitTimeout.
func (obj *StorageST) StreamChannelSDP(streamID string) ([]byte, error) {
	deadline := time.Now().Add(streamCodecWaitTimeout)
	for {
		obj.mutex.RLock()
		tmp, ok := obj.Streams[streamID]
		if !ok {
			obj.mutex.RUnlock()
			return nil, ErrorStreamNotFound
		}
		if len(tmp.sdp) > 0 {
			ret := append([]byte(nil), tmp.sdp...)
			obj.mutex.RUnlock()
			return ret, nil
		}
		running := tmp.runLock
		obj.mutex.RUnlock()

		if !running {
			return nil, ErrorStreamChannelStopped
		}
		if !time.Now().Before(deadline) {
			return nil, ErrorStreamChannelCodecNotFound
		}
		time.Sleep(streamCodecPollInterval)
	}
}

// detachClientLocked removes one viewer and releases its stop channel.
// obj.mutex must be held. It is safe to call more than once for the same
// viewer (the stopped flag guards the channel close).
func (obj *StorageST) detachClientLocked(s *StreamST, cid string, c *ClientST) {
	if cid != "" {
		delete(s.clients, cid)
	}
	if c == nil || c.stopped {
		return
	}
	c.stopped = true
	if c.stop != nil {
		close(c.stop)
	}
}

// detachAllClientsLocked removes and stops all viewers of a session.
// obj.mutex must be held.
func (obj *StorageST) detachAllClientsLocked(s *StreamST) {
	for cid, c := range s.clients {
		obj.detachClientLocked(s, cid, c)
	}
}
