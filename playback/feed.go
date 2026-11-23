package playback

import (
	"fmt"

	. "github.com/sqweek/sqribe/core/types"
)

// heldNote tracks a note currently ringing so it can be released at its end.
type heldNote struct {
	end     FrameN
	channel uint8
	pitch   uint8
}

// beatCursor walks the metronome frames for one pass over the range.
type eventLists struct {
	beats []FrameN
	notes []NoteEvent
	// bIdx/nIdx point at the next not-yet-processed entry.
	bIdx, nIdx int
}

func (s *session) buildEvents(rng Range, cur FrameN) *eventLists {
	ev := &eventLists{
		beats: s.player.events.Beats(rng.Min, rng.Max),
		notes: s.player.events.Notes(rng.Min, rng.Max),
	}
	ev.advance(cur)
	return ev
}

// advance moves cursors past events that already occurred before frame cur.
func (ev *eventLists) advance(cur FrameN) {
	for ev.bIdx < len(ev.beats) && ev.beats[ev.bIdx] < cur {
		ev.bIdx++
	}
	for ev.nIdx < len(ev.notes) && ev.notes[ev.nIdx].Start < cur {
		ev.nIdx++
	}
}

/* feed consumes prefetched blocks, renders synth events 64 frames at a time,
** mixes them with the waveform and pushes the result to the sink. It owns all
** note-on/note-off bookkeeping and is the single place where events restart on
** a wrap, guaranteeing no note or metronome click leaks across a boundary. */
func (s *session) feed() {
	defer s.cleanup()

	src := s.player.src
	synth := s.player.synth
	sink := s.player.sink
	nchan := src.Channels()
	blockSamples := int(BlockFrames) * nchan
	mbuf := make([]int16, blockSamples)

	var cur block
	var haveBlock bool
	var cutoff FrameN
	var held []heldNote
	beatOn := false
	var woodChannel, woodPitch uint8 = 255, 255
	var ev *eventLists

	// release everything still ringing; idempotent and safe at shutdown/wrap.
	releaseAll := func() {
		for _, h := range held {
			synth.NoteOff(h.channel, h.pitch)
		}
		held = held[:0]
		if beatOn {
			synth.NoteOff(woodChannel, woodPitch)
			beatOn = false
		}
		synth.AllNotesOff()
	}

	for {
		if !haveBlock {
			select {
			case b, ok := <-s.samples:
				if !ok {
					return // prefetch exited (stop or source failure)
				}
				cur = b
			case <-s.ctx.Done():
				return
			}
			if len(cur.buf) == 0 || len(cur.buf)%blockSamples != 0 {
				s.fail(fmt.Errorf("%w: block size %d not a multiple of %d frames",
					ErrSource, len(cur.buf), BlockFrames))
				return
			}
			// apply any score edits accumulated since the last block boundary
			s.drainChanges(&ev, cur.rng, cur.frame)
			if ev == nil {
				ev = s.buildEvents(cur.rng, cur.frame)
			}
			if cur.wrapped {
				// looping back: release anything from the previous pass and
				// replay beats/notes from the top of the range.
				releaseAll()
				ev = s.buildEvents(cur.rng, cur.frame)
				sink.Rewind(cur.frame)
			}
			cutoff = cur.frame
			haveBlock = true
		}

		// process one 64-frame sub-block
		buf := cur.buf[:blockSamples]
		cur.buf = cur.buf[blockSamples:]
		cutoff += BlockFrames

		// note-offs first so back-to-back identical pitches retrigger cleanly
		held = releaseDue(synth, held, cutoff)
		// metronome
		beatOn = s.runMetronome(synth, ev, &woodChannel, &woodPitch, beatOn, cutoff)
		// score notes
		held = s.triggerNotes(synth, ev, held, cutoff)

		synth.WriteFrames(mbuf)
		levels := s.player.events.Levels()
		mp, wp := mix(buf, mbuf, levels)
		s.recordPeaks(mp, wp)

		if err := sink.Append(mbuf); err != nil {
			s.fail(mkErr(ErrSink, err))
			return
		}

		if len(cur.buf) == 0 {
			haveBlock = false
			if cur.tail {
				// non-looping end: stop after the crossfaded tail is audible
				s.fail(nil)
				return
			}
		}
	}
}

/* drainChanges applies all queued score edits, rebuilding the event lists once
** for the coalesced batch and preserving playback position. */
func (s *session) drainChanges(ev **eventLists, rng Range, cur FrameN) {
	pending := false
	for {
		select {
		case c := <-s.changeCh:
			_ = c
			pending = true
		default:
			if pending {
				*ev = s.buildEvents(rng, cur)
			}
			return
		}
	}
}

func releaseDue(synth Synth, held []heldNote, cutoff FrameN) []heldNote {
	for j := len(held) - 1; j >= 0; j-- {
		if held[j].end < cutoff {
			synth.NoteOff(held[j].channel, held[j].pitch)
			held = append(held[:j], held[j+1:]...)
		}
	}
	return held
}

func (s *session) runMetronome(synth Synth, ev *eventLists, woodCh, woodPitch *uint8, on bool, cutoff FrameN) bool {
	const metronomeInst, metronomeNote, metronomeVel = uint8(255), uint8(95), uint8(120)
	if on {
		// a click triggered in a previous sub-block lasts exactly one block
		synth.NoteOff(*woodCh, *woodPitch)
		on = false
	}
	if s.player.events.Levels().MuteMetronome {
		return on
	}
	for ev.bIdx < len(ev.beats) && ev.beats[ev.bIdx] < cutoff {
		if !on {
			ch := synth.Inst(metronomeInst)
			*woodCh = ch
			*woodPitch = metronomeNote
			synth.NoteOn(ch, metronomeNote, metronomeVel)
			on = true
		}
		ev.bIdx++
	}
	return on
}

func (s *session) triggerNotes(synth Synth, ev *eventLists, held []heldNote, cutoff FrameN) []heldNote {
	for ev.nIdx < len(ev.notes) && ev.notes[ev.nIdx].Start < cutoff {
		n := ev.notes[ev.nIdx]
		ev.nIdx++
		if n.Muted {
			continue
		}
		ch := synth.Inst(n.Inst)
		synth.NoteOn(ch, n.Pitch, n.Velocity)
		held = append(held, heldNote{end: n.End, channel: ch, pitch: n.Pitch})
	}
	return held
}

func (s *session) recordPeaks(midiPeak, wavePeak float64) {
	s.peakMu.Lock()
	if midiPeak > s.midiPeak {
		s.midiPeak = midiPeak
	}
	if wavePeak > s.wavePeak {
		s.wavePeak = wavePeak
	}
	s.peakMu.Unlock()
}

/* mix combines waveform samples buf with rendered synth samples mbuf using the
** supplied gain/mute snapshot, writing the result back into mbuf. It returns
** the pre-master peak amplitudes of the midi and wave contributions so the
** caller can update level meters. A simple hard limiter guards against int16
** overflow. */
func mix(buf, mbuf []int16, lv MixLevels) (midiPeak, wavePeak float64) {
	wg, mg := 1.0, 1.0
	if lv.MuteWave {
		wg = 0
	} else {
		wg = lv.Wave
	}
	if lv.MuteMidi {
		mg = 0
	} else {
		mg = lv.Midi
	}
	master := lv.Master
	for j := 0; j < len(mbuf); j++ {
		w := wg * float64(buf[j])
		m := mg * float64(mbuf[j])
		if a := absf(m); a > midiPeak {
			midiPeak = a
		}
		if a := absf(w); a > wavePeak {
			wavePeak = a
		}
		sum := master * (w + m)
		if sum > 32767 {
			sum = 32767
		} else if sum < -32768 {
			sum = -32768
		}
		mbuf[j] = int16(sum)
	}
	return midiPeak, wavePeak
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

/* cleanup runs exactly once when the feeder exits (normal end, stop, or error).
** It releases every held note, stops the sink, waits for the prefetch goroutine
** to finish and detaches the session so a new playback can start immediately. */
func (s *session) cleanup() {
	s.markStopping()
	s.cancel()
	// ensure no note survives; the feeder's own held list is released here too
	s.player.synth.AllNotesOff()
	s.player.sink.Stop()
	// prefetch closes s.samples and prefetchDone on its way out; wait for it
	<-s.prefetchDone
	close(s.feederDone)

	s.player.mu.Lock()
	if s.player.session == s {
		s.player.session = nil
	}
	s.player.mu.Unlock()
}

func mkErr(wrap, cause error) error {
	if cause == nil {
		return wrap
	}
	return fmt.Errorf("%w: %v", wrap, cause)
}
