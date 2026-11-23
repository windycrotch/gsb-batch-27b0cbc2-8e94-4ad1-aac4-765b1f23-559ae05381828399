// Package capture implements the event driven capture loop shared by the
// audio capture examples.
//
// The loop is deliberately free of Windows specific code so that it can be
// exercised with fake event sources and fake buffer sources on any platform.
package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// EventSource represents the audio ready event which drives the capture loop.
//
// Wait must block until the event is signaled, ctx is done or waiting fails.
// Implementations must return from Wait before Close is called, and must not
// leave a waiter behind after returning. The session calls Wait from a single
// goroutine, so at most one waiter is active at any time.
type EventSource interface {
	Wait(ctx context.Context) error
	Close() error
}

// BufferSource provides access to the capture endpoint buffer.
//
// A successful Acquire must be followed by exactly one Release call with the
// same frame count, including when Acquire returns zero frames. Data returned
// by Acquire is owned by the caller, so it remains valid after Release.
//
// Acquire may return ErrNoData to report that no buffer is available yet. In
// that case no buffer was acquired and Release must not be called.
type BufferSource interface {
	Acquire() (frames uint32, data []byte, err error)
	Release(frames uint32) error
}

// ErrNoData reports that no capture buffer was available. It is not a failure:
// the capture loop simply waits for the next audio event.
var ErrNoData = errors.New("capture: no data available")

// Stopper is an audio client which must be stopped when the session ends.
type Stopper interface {
	Stop() error
}

// Session drives an event driven capture loop.
//
// A Session is meant to be used once: Run captures audio until the duration is
// reached, ctx is done, or the event source or buffer source fails. In all
// cases the session stops every Stopper, closes the EventSource, and returns
// the first error it encountered. Cleanup is performed at most once.
type Session struct {
	// Events is the audio ready event which drives the loop.
	Events EventSource
	// Buffers is the source of captured audio blocks.
	Buffers BufferSource
	// Output receives the captured PCM data. If nil, captured data is
	// discarded, but the duration is still accounted for.
	Output io.Writer
	// BytesPerSecond is the average number of bytes captured per second. It is
	// used to translate Duration into a number of captured bytes.
	BytesPerSecond uint32
	// Duration is the maximum amount of audio to capture. Zero or a negative
	// value means that capture continues until ctx is done.
	Duration time.Duration
	// Stoppers are stopped in order when Run returns. Note that the bitrate is
	// computed from the input format, not from the amount of data actually
	// written to Output.
	Stoppers []Stopper

	once       sync.Once
	finalError error
}

// Run captures audio until the configured duration has been reached, ctx is
// done, or a failure occurs.
//
// Returning because ctx was canceled is not an error: a user interrupt still
// produces a complete output stream. If a buffer has been acquired it is
// always released before Run returns.
func (s *Session) Run(ctx context.Context) (err error) {
	if s.Events == nil {
		return s.finish(fmt.Errorf("capture: nil event source"))
	}
	if s.Buffers == nil {
		return s.finish(fmt.Errorf("capture: nil buffer source"))
	}
	if s.Duration > 0 && s.BytesPerSecond == 0 {
		return s.finish(fmt.Errorf("capture: bytes per second must not be zero when a duration is set"))
	}

	var captured int64

	for {
		// The context is checked before waiting and again after the wait
		// returns. Once an interrupt has been observed, no new data is taken
		// from the capture client, but data acquired earlier is already
		// complete and has been released.
		if ctx.Err() != nil {
			break
		}
		if s.durationReached(captured) {
			break
		}

		if err = s.Events.Wait(ctx); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return s.finish(err)
		}
		if ctx.Err() != nil {
			break
		}
		if s.durationReached(captured) {
			break
		}

		frames, data, acquireErr := s.Buffers.Acquire()
		if acquireErr != nil {
			if errors.Is(acquireErr, ErrNoData) {
				if ctx.Err() != nil {
					break
				}
				continue
			}
			// A failed Acquire does not hand out a buffer, so there is
			// nothing to release.
			return s.finish(acquireErr)
		}

		// Write before releasing so that data handed to the output is never
		// lost because of a later failure. The buffer is released even when
		// writing fails. The duration is accounted from the captured audio,
		// not from the number of bytes the writer accepted.
		captured += int64(len(data))

		var writeErr error
		if len(data) > 0 && s.Output != nil {
			_, writeErr = s.Output.Write(data)
		}

		releaseErr := s.Buffers.Release(frames)

		if writeErr != nil {
			return s.finish(writeErr)
		}
		if releaseErr != nil {
			return s.finish(releaseErr)
		}
	}

	return s.finish(nil)
}

// durationReached reports whether the captured byte count covers Duration.
func (s *Session) durationReached(captured int64) bool {
	if s.Duration <= 0 || s.BytesPerSecond <= 0 {
		return false
	}

	bytesPerSecond := int64(s.BytesPerSecond)
	whole := captured / bytesPerSecond
	remainder := captured % bytesPerSecond

	elapsed := time.Duration(whole)*time.Second +
		time.Duration(remainder)*time.Second/time.Duration(bytesPerSecond)

	return elapsed >= s.Duration
}

// finish stops the audio clients and closes the event source exactly once,
// regardless of how many times it is called. The first error encountered is
// returned to the caller; later failures do not prevent the remaining cleanup
// from running.
func (s *Session) finish(cause error) error {
	s.once.Do(func() {
		for _, stopper := range s.Stoppers {
			if stopper == nil {
				continue
			}
			if err := stopper.Stop(); err != nil && cause == nil {
				cause = err
			}
		}
		if s.Events != nil {
			if err := s.Events.Close(); err != nil && cause == nil {
				cause = err
			}
		}
		s.finalError = cause
	})

	return s.finalError
}
