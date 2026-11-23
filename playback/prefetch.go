package playback

import (
	"context"

	. "github.com/sqweek/sqribe/core/types"
)

/* prefetch reads sample blocks from the source slightly ahead of the feeder.
** It re-evaluates the loop range on every iteration (and whenever SetRange
** posts an update) and clamps its cursor into the range before every read, so a
** mid-playback range change can never cause an out-of-bounds read. Source
** errors or context cancellation end the session cleanly. */
func (s *session) prefetch() {
	defer close(s.samples)
	defer close(s.prefetchDone)

	src := s.player.src
	frame := s.startPos
	bufsiz := PrefetchFrames

	for {
		if s.ctx.Err() != nil {
			return
		}
		// absorb the latest pending range update (latest-wins, never blocks)
		select {
		case rng := <-s.rangeCh:
			s.applyRange(rng)
		default:
		}
		rng := s.currentRange()

		// a shrinking range may leave the cursor outside; clamp before reading.
		if frame < rng.Min || frame > rng.Max {
			frame = rng.Min
		}

		blk, err := s.readBlock(src, rng, frame, bufsiz)
		if err != nil {
			if err != context.Canceled && err != context.DeadlineExceeded {
				s.fail(mkErr(ErrSource, err))
			} else {
				s.fail(nil)
			}
			return
		}

		// hand off unless the feeder has already exited
		select {
		case s.samples <- blk:
		case <-s.ctx.Done():
			return
		case <-s.feederDone:
			return
		}

		// advance; the tail block (crossfade at the range end) always means the
		// cursor has reached the range boundary: wrap or terminate accordingly.
		nf := src.ToFrame(SampleN(len(blk.buf) / src.Channels()))
		if blk.tail || blk.wrapped {
			if !s.loop {
				return
			}
			frame = blk.rng.Min
			continue
		}
		frame += nf
		rng = s.currentRange()
		if frame > rng.Max {
			if !s.loop {
				return
			}
			frame = rng.Min
		}
	}
}

/* applyRange publishes a new range, clamping it to something the source can
** satisfy is the caller's responsibility; here we only store it. */
func (s *session) applyRange(rng Range) {
	s.rangeMu.Lock()
	s.rng = rng
	s.rangeMu.Unlock()
}

/* readBlock fetches one prefetch unit. When the block would run past the range
** end it instead builds the tail block: samples up to the end plus a crossfade
** back to the range start, and marks whether the session should terminate
** (non-looping) or wrap. */
func (s *session) readBlock(src SampleSource, rng Range, frame, bufsiz FrameN) (block, error) {
	ctx := s.ctx
	if frame+bufsiz-1 <= rng.Max {
		samples, err := src.Frames(ctx, frame, frame+bufsiz-1)
		if err != nil {
			return block{}, err
		}
		return block{frame: frame, rng: rng, buf: samples}, nil
	}

	// partial block to the end of the range
	endSamples, err := src.Frames(ctx, frame, rng.Max)
	if err != nil {
		return block{}, err
	}
	startSamples, err := src.Frames(ctx, rng.Min, rng.Min)
	if err != nil {
		return block{}, err
	}
	nchan := FrameN(src.Channels())
	avail := FrameN(len(endSamples)) / nchan
	// pad up to a whole number of 64-frame blocks; minimum 20 frames of fade
	pad := 19 + (BlockFrames-((avail+19)%BlockFrames))%BlockFrames
	if pad < 20 {
		pad += BlockFrames
	}
	fade := crossfade(endSamples[len(endSamples)-len(startSamples):], startSamples, pad, nchan)
	out := make([]int16, len(endSamples)+len(fade))
	copy(out, endSamples)
	copy(out[len(endSamples):], fade)

	tail := !s.loop
	return block{
		frame:   frame,
		rng:     rng,
		buf:     out,
		wrapped: s.loop,
		tail:    tail,
	}, nil
}

/* crossfade produces `steps` frames interpolating linearly from the tail of
** `from` down to zero and from zero up to the start of `to`. Both inputs hold
** one frame (nchan interleaved samples). */
func crossfade(from, to []int16, steps, nchan FrameN) []int16 {
	out := make([]int16, nchan*steps)
	for i := FrameN(0); i < steps; i++ {
		α := 1.0 - float64(i+1)/float64(steps+1)
		for j := FrameN(0); j < nchan; j++ {
			if α > 0.5 {
				out[nchan*i+j] = int16(float64(from[j]) * 2 * (α - 0.5))
			} else {
				out[nchan*i+j] = int16(float64(to[j]) * 2 * (0.5 - α))
			}
		}
	}
	return out
}
