// RTMP session publisher methods

package main

import (
	"container/list"
	"crypto/subtle"
)

// Starts sending to idle players
// Call only for publishers
func (s *RTMPSession) StartIdlePlayers() {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	// Start idle players
	idlePlayers := s.server.GetIdlePlayers(s.channel)

	for i := 0; i < len(idlePlayers); i++ {
		if subtle.ConstantTimeCompare([]byte(s.key), []byte(idlePlayers[i].key)) == 1 {
			player := idlePlayers[i]

			LogRequest(player.id, player.ip, "PLAY START '"+player.channel+"'")

			player.SendMetadata(s.metaData, 0)
			player.SendAudioCodecHeader(s.audioCodec, s.aacSequenceHeader, 0)
			player.SendVideoCodecHeader(s.videoCodec, s.avcSequenceHeader, 0)

			if !player.gopPlayNo && s.rtmpGopCache.Len() > 0 {
				for t := s.rtmpGopCache.Front(); t != nil; t = t.Next() {
					chunks := t.Value
					switch x := chunks.(type) {
					case *RTMPPacket:
						player.SendCachePacket(x)
					}
				}
			}

			// Player state flags are always mutated under the server mutex so
			// concurrent end/leave operations cannot interleave with them.
			s.server.setPlayerPlaying(player)

			if player.gopPlayClear {
				s.rtmpGopCache = list.New()
				s.gopCacheSize = 0
				s.gopCacheDisabled = true
			}
		} else {
			LogRequest(idlePlayers[i].id, idlePlayers[i].ip, "Error: Invalid stream key provided")
			idlePlayers[i].SendStatusMessage(s.playStreamId, "error", "NetStream.Play.BadName", "Invalid stream key provided")
			// Detach before closing: a killed session must never remain
			// referenced by the channel, and its slot must be released so the
			// channel can later be cleaned up correctly.
			badPlayer := idlePlayers[i]
			s.server.RemovePlayer(s.channel, badPlayer.key, badPlayer)
			badPlayer.Kill()
		}

	}
}

// Starts a specific player
// Call only for publishers
// player - The player session
func (s *RTMPSession) StartPlayer(player *RTMPSession) {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	if !s.isPublishing {
		s.server.setPlayerIdle(player)
		LogRequest(player.id, player.ip, "PLAY IDLE '"+player.channel+"'")
		return
	}

	LogRequest(player.id, player.ip, "PLAY START '"+player.channel+"'")

	player.SendMetadata(s.metaData, 0)
	player.SendAudioCodecHeader(s.audioCodec, s.aacSequenceHeader, 0)
	player.SendVideoCodecHeader(s.videoCodec, s.avcSequenceHeader, 0)

	if !player.gopPlayNo && s.rtmpGopCache.Len() > 0 {
		for t := s.rtmpGopCache.Front(); t != nil; t = t.Next() {
			chunks := t.Value
			switch x := chunks.(type) {
			case *RTMPPacket:
				player.SendCachePacket(x)
			}
		}
	}

	s.server.setPlayerPlaying(player)

	if player.gopPlayClear {
		s.rtmpGopCache = list.New()
		s.gopCacheSize = 0
		s.gopCacheDisabled = true
	}
}

// Resumes a player that was paused
// Call only for publishers
// player - The player session
func (s *RTMPSession) ResumePlayer(player *RTMPSession) {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	player.SendAudioCodecHeader(s.audioCodec, s.aacSequenceHeader, s.clock)
	player.SendVideoCodecHeader(s.videoCodec, s.avcSequenceHeader, s.clock)
}

// Finishes a publishing session
// Call only for publishers
// isClose - True if it was closed due to a disconnection
//
// Exactly-once: the whole teardown (publisher removal, player -> idle
// transition and end notifications) runs only while s.isPublishing is true
// and is cleared inside the same guarded section. A duplicate invocation
// (e.g. deleteStream followed by connection close) performs nothing, so
// viewers receive a single end notification and the channel state is never
// rewritten a second time.
func (s *RTMPSession) EndPublish(isClose bool) {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	if !s.isPublishing {
		return
	}

	LogRequest(s.id, s.ip, "PUBLISH END '"+s.channel+"'")

	if !isClose {
		s.SendStatusMessage(s.publishStreamId, "status", "NetStream.Unpublish.Success", s.GetStreamPath()+" is now unpublished.")
	}

	// Atomically clear the publisher and move every active player back to
	// idle. The snapshot only contains players that were actually playing, so
	// the end notification is sent exactly once per viewer.
	players := s.server.EndPublisher(s.channel)

	for i := 0; i < len(players); i++ {
		LogRequest(players[i].id, players[i].ip, "PLAY IDLE '"+players[i].channel+"'")
		players[i].SendStatusMessage(players[i].playStreamId, "status", "NetStream.Play.UnpublishNotify", "stream is now unpublished.")
		players[i].SendStreamStatus(STREAM_EOF, players[i].playStreamId)
	}

	s.rtmpGopCache = list.New()

	// Mark the session as no longer publishing inside the same guarded section
	// so a concurrent re-entry observes the finished state.
	s.isPublishing = false

	// Send event
	if s.server.websocketControlConnection != nil {
		if s.server.websocketControlConnection.PublishEnd(s.channel, s.stream_id) {
			LogDebugSession(s.id, s.ip, "Stop event sent")
		} else {
			LogDebugSession(s.id, s.ip, "Could not send stop event")
		}
	} else {
		if s.SendStopCallback() {
			LogDebugSession(s.id, s.ip, "Stop event sent")
		} else {
			LogDebugSession(s.id, s.ip, "Could not send stop event")
		}
	}
}

// Sets the clock for a publishing session
// clock - The value of the clock
func (s *RTMPSession) SetClock(clock int64) {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	s.clock = clock
}

// Sets the stream metadata that is being publishing
// metaData - The metadata
func (s *RTMPSession) SetMetaData(metaData []byte) {
	s.publish_mutex.Lock()
	defer s.publish_mutex.Unlock()

	if !s.isPublishing {
		return
	}

	s.metaData = metaData

	players := s.server.GetPlayers(s.channel)

	for i := 0; i < len(players); i++ {
		players[i].SendMetadata(metaData, 0)
	}
}
