// WebRTC sources management

package main

// Checks if the node has a WebRTC source
// for the specified Stream Id (sid)
func (node *WebRTC_CDN_Node) resolveSource(sid string) bool {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	return node.sources[sid] != nil
}

// Registers a WebRTC source.
//
// If another source is already publishing the same stream id, the new source
// replaces it: the old source is closed and notified, and every sink attached
// to the stream is sent back to STANDBY with its old tracks invalidated. When
// the new source becomes ready the sinks receive the new tracks.
func (node *WebRTC_CDN_Node) registerSource(source *WRTC_Source) {
	node.mutexStatus.Lock()

	oldSource := node.sources[source.sid]

	// Replace the registered source before releasing the lock so there is
	// never a moment where the same stream has two active registered sources.
	node.sources[source.sid] = source

	// Remove any relays for that source
	if node.relays[source.sid] != nil {
		node.relays[source.sid].close()
		delete(node.relays, source.sid)
	}

	// Remove all the senders
	if node.senders[source.sid] != nil {
		for _, sender := range node.senders[source.sid] {
			sender.close()
		}
		delete(node.senders, source.sid)
	}

	// Snapshot the sinks so they can be notified outside the node lock
	var affectedSinks []*WRTC_Sink
	if m := node.sinks[source.sid]; m != nil {
		for _, sink := range m {
			affectedSinks = append(affectedSinks, sink)
		}
	}

	node.mutexStatus.Unlock()

	if oldSource != nil && oldSource != source {
		oldVideo, oldAudio := oldSource.tracks()

		// First invalidate the old tracks on every sink, sending them back to
		// STANDBY. This must precede closing the old source so a teardown
		// callback from it cannot re-attach stale tracks to the sinks.
		for _, sink := range affectedSinks {
			sink.onTracksClosed(oldVideo, oldAudio)
		}

		// Then close the old source and notify its connection. deregister is
		// false because the map entry has already been replaced by the new
		// source; the identity guard in onSourceClosed would reject it anyway.
		oldSource.close(true, false)
	}

	// Announce to other nodes (no-op in standalone mode)
	node.sendInfoMessage(REDIS_BROADCAST_CHANNEL, source.sid)
}

// Called when a WebRTC source is ready
// This means the tracks can be played
func (node *WebRTC_CDN_Node) onSourceReady(source *WRTC_Source) {
	node.mutexStatus.Lock()

	// Ignore stale notifications from a source that was closed or replaced
	// before its tracks finished arriving.
	if source.closed || node.sources[source.sid] != source {
		node.mutexStatus.Unlock()
		return
	}

	source.ready = true

	// Snapshot the sinks and senders so they can be notified outside the node
	// lock (each takes its own lock and starts a peer transport).
	var affectedSinks []*WRTC_Sink
	if m := node.sinks[source.sid]; m != nil {
		for _, sink := range m {
			affectedSinks = append(affectedSinks, sink)
		}
	}

	var affectedSenders []*WRTC_Source_Sender
	if m := node.senders[source.sid]; m != nil {
		for _, sender := range m {
			affectedSenders = append(affectedSenders, sender)
		}
	}

	video := source.localTrackVideo
	audio := source.localTrackAudio

	node.mutexStatus.Unlock()

	for _, sink := range affectedSinks {
		sink.onTracksReady(video, audio)
	}

	for _, sender := range affectedSenders {
		sender.onTracksReady(video, audio)
	}
}

// Called when the WebRTC source closes by itself (by the client, a disconnection, etc)
// Do not call from another status methods
func (node *WebRTC_CDN_Node) onSourceClosed(source *WRTC_Source) {
	node.mutexStatus.Lock()

	// Identity guard: if the source registered for this stream is not this
	// one, the source was replaced and the replacement path owns the cleanup.
	// Removing anything here would corrupt the new publisher's registration.
	if node.sources[source.sid] != source {
		node.mutexStatus.Unlock()
		return
	}

	delete(node.sources, source.sid)

	// Close and remove all the senders
	if node.senders[source.sid] != nil {
		for _, sender := range node.senders[source.sid] {
			sender.close()
		}

		delete(node.senders, source.sid)
	}

	// Any sinks waiting, tell them the tracks are closed
	var affectedSinks []*WRTC_Sink
	if m := node.sinks[source.sid]; m != nil {
		for _, sink := range m {
			affectedSinks = append(affectedSinks, sink)
		}
	}

	node.mutexStatus.Unlock()

	for _, sink := range affectedSinks {
		sink.onTracksClosed(source.localTrackVideo, source.localTrackAudio)
	}
}
