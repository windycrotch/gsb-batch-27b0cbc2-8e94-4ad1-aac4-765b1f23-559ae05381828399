// WebRTC sinks management

package main

// Generates an unique ID for each sink
func (node *WebRTC_CDN_Node) getSinkID() uint64 {
	node.mutexSinkCount.Lock()
	defer node.mutexSinkCount.Unlock()

	node.sinkCount++

	return node.sinkCount
}

// Registers a sink
func (node *WebRTC_CDN_Node) registerSink(sink *WRTC_Sink) {
	node.mutexStatus.Lock()

	if node.sinks[sink.sid] == nil {
		node.sinks[sink.sid] = make(map[uint64]*WRTC_Sink)
	}

	node.sinks[sink.sid][sink.sinkId] = sink

	// Capture the tracks (if any are already available) while holding the
	// node lock; notify the sink outside the lock so the sink can take its own
	// lock and start its peer transport without nested locking.
	var video mediaTrack
	var audio mediaTrack
	notify := false

	if node.sources[sink.sid] != nil && node.sources[sink.sid].ready {
		video = node.sources[sink.sid].localTrackVideo
		audio = node.sources[sink.sid].localTrackAudio
		notify = true
	} else if node.relays[sink.sid] != nil && node.relays[sink.sid].ready {
		video = node.relays[sink.sid].localTrackVideo
		audio = node.relays[sink.sid].localTrackAudio
		notify = true
	}

	node.mutexStatus.Unlock()

	if notify {
		sink.onTracksReady(video, audio)
		return
	}

	// Can't find any source, maybe other node has it?
	// Announce to other nodes to create the relay
	node.sendResolveMessage(sink.sid)
}

// Removes a sink
func (node *WebRTC_CDN_Node) removeSink(sink *WRTC_Sink) {
	node.mutexStatus.Lock()

	if node.sinks[sink.sid] == nil || node.sinks[sink.sid][sink.sinkId] == nil {
		node.mutexStatus.Unlock()
		return
	}

	delete(node.sinks[sink.sid], sink.sinkId)

	var relayToClose *WRTC_Relay
	if len(node.sinks[sink.sid]) == 0 {
		delete(node.sinks, sink.sid)

		// No more sinks for that stream means there is no need for relays
		if node.relays[sink.sid] != nil {
			relayToClose = node.relays[sink.sid]
			delete(node.relays, sink.sid)
		}
	}

	node.mutexStatus.Unlock()

	// Close the relay outside the node lock.
	if relayToClose != nil {
		relayToClose.close()
	}
}
