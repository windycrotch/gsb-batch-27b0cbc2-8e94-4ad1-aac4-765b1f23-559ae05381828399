package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"

	"github.com/rezmoss/citychime/internal/chime"
)

// streamPlayer plays one beep.Streamer.
//
// It is an interface so playback completion and cancellation can be tested
// without a sound card, but the production implementation is the beep speaker.
type streamPlayer interface {
	// Play blocks until s has been fully consumed (the streamer itself reports
	// completion) or until ctx is cancelled, whichever happens first. It must
	// not rely on an estimated duration.
	Play(ctx context.Context, s beep.Streamer) error
	// Close releases the audio device.
	Close() error
}

// beepLoader decodes the chime file and plays it through beep's speaker.
//
// Decoding, resource lookup and speaker setup happen on first successful use and
// are then cached. A failure is never cached: the next call retries the lookup,
// the decode and the device setup, so a chime file or an audio device that
// becomes available later (or a transient device error) recovers on the next
// trigger instead of disabling the sound forever.
//
// Load and Close are only ever called from the scheduler goroutine.
type beepLoader struct {
	resolve   func() (string, error)
	decode    func(path string) (*beep.Buffer, error)
	newOutput func(format beep.Format) (streamPlayer, error)

	mu     sync.Mutex
	buffer *beep.Buffer
	output streamPlayer
}

func newBeepLoader(resolve func() (string, error)) *beepLoader {
	return &beepLoader{
		resolve:   resolve,
		decode:    decodeChime,
		newOutput: initSpeaker,
	}
}

// Load implements chime.Loader.
func (l *beepLoader) Load() (chime.Sound, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.buffer != nil {
		return &beepSound{buffer: l.buffer, output: l.output, loader: l}, nil
	}

	path, err := l.resolve()
	if err != nil {
		return nil, err
	}
	buffer, err := l.decode(path)
	if err != nil {
		return nil, err
	}
	output, err := l.newOutput(buffer.Format())
	if err != nil {
		return nil, fmt.Errorf("starting audio output: %w", err)
	}

	l.buffer = buffer
	l.output = output
	return &beepSound{buffer: buffer, output: output, loader: l}, nil
}

// invalidate drops the cached buffer and audio device after a playback failure,
// so the next Load reopens both. Without this a device that was lost (for
// example a USB headset being unplugged) would stay broken for the rest of the
// process lifetime.
func (l *beepLoader) invalidate() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.output != nil {
		_ = l.output.Close()
	}
	l.output = nil
	l.buffer = nil
}

// Close implements chime.Loader.
func (l *beepLoader) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.output == nil {
		return nil
	}
	err := l.output.Close()
	l.output = nil
	l.buffer = nil
	return err
}

func decodeChime(path string) (*beep.Buffer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening chime audio %s: %w", path, err)
	}
	defer f.Close()

	streamer, format, err := mp3.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding chime audio %s: %w", path, err)
	}
	defer streamer.Close()

	buffer := beep.NewBuffer(format)
	buffer.Append(streamer)
	if buffer.Len() == 0 {
		return nil, fmt.Errorf("decoding chime audio %s: file contains no audio", path)
	}
	return buffer, nil
}

func initSpeaker(format beep.Format) (streamPlayer, error) {
	if err := speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10)); err != nil {
		return nil, err
	}
	return speakerOutput{}, nil
}

// speakerOutput plays through beep's global speaker.
type speakerOutput struct{}

// Play starts s and waits for the player to report that the streamer is drained.
//
// Completion comes from beep itself: the callback streamer runs only after s has
// produced its last sample, so no duration estimate is involved. On cancellation
// everything queued in the speaker is dropped so the chime goes silent at once.
func (speakerOutput) Play(ctx context.Context, s beep.Streamer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	done := make(chan struct{})
	var once sync.Once
	notifyDone := func() { once.Do(func() { close(done) }) }
	speaker.Play(beep.Seq(s, beep.Callback(notifyDone)))

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Clear takes the speaker lock itself, so it must not be called while
		// holding the lock.
		speaker.Clear()
		return ctx.Err()
	}
}

func (speakerOutput) Close() error {
	speaker.Close()
	return nil
}

// beepSound is one decoded chime, replayable strike by strike.
type beepSound struct {
	buffer *beep.Buffer
	output streamPlayer
	loader *beepLoader
}

// PlayOnce plays the whole chime and returns when playback has really finished,
// or as soon as ctx is cancelled.
func (s *beepSound) PlayOnce(ctx context.Context) error {
	err := s.output.Play(ctx, s.buffer.Streamer(0, s.buffer.Len()))
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// The device may be gone; make the next trigger reopen it.
		s.loader.invalidate()
	}
	return err
}
