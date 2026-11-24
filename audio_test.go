package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faiface/beep"

	"github.com/rezmoss/citychime/internal/chime"
)

// fakePlayer stands in for the speaker. It plays a streamer by simply draining
// it, so completion is still reported by the streamer (as in production) rather
// than by a timer.
type fakePlayer struct {
	initErr error
	closed  atomic.Int32
	// playErr makes Play return an error instead of draining.
	playErr error
	// holdBlocks makes Play wait for the context even after draining.
	holdBlocks bool

	mu    sync.Mutex
	plays []beep.Streamer
}

func (p *fakePlayer) Play(ctx context.Context, s beep.Streamer) error {
	p.mu.Lock()
	p.plays = append(p.plays, s)
	p.mu.Unlock()

	if p.playErr != nil {
		return p.playErr
	}
	if p.holdBlocks {
		<-ctx.Done()
		return ctx.Err()
	}

	samples := make([][2]float64, 64)
	for {
		n, ok := s.Stream(samples)
		_ = n
		if !ok {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (p *fakePlayer) Close() error {
	p.closed.Add(1)
	return nil
}

func (p *fakePlayer) playCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.plays)
}

// newTestLoader builds a beepLoader whose decoder and player are fakes.
func newTestLoader(resolve func() (string, error), player *fakePlayer, decodeErr error) (*beepLoader, *atomic.Int32) {
	var decodes atomic.Int32
	l := &beepLoader{
		resolve: resolve,
		decode: func(string) (*beep.Buffer, error) {
			decodes.Add(1)
			if decodeErr != nil {
				return nil, decodeErr
			}
			format := beep.Format{SampleRate: beep.SampleRate(44100), NumChannels: 2, Precision: 2}
			buffer := beep.NewBuffer(format)
			buffer.Append(beep.Silence(100))
			return buffer, nil
		},
		newOutput: func(beep.Format) (streamPlayer, error) {
			if player.initErr != nil {
				return nil, player.initErr
			}
			return player, nil
		},
	}
	return l, &decodes
}

func TestBeepLoaderRetriesUntilResourceAppears(t *testing.T) {
	player := &fakePlayer{}
	available := atomic.Bool{}
	loader, decodes := newTestLoader(func() (string, error) {
		if !available.Load() {
			return "", errors.New(`resource "chime.mp3" not found`)
		}
		return "chime.mp3", nil
	}, player, nil)

	if _, err := loader.Load(); err == nil {
		t.Fatalf("Load succeeded while the resource was missing")
	}
	if decodes.Load() != 0 {
		t.Fatalf("decode attempted %d times for a missing resource", decodes.Load())
	}

	available.Store(true)
	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load after recovery: %v", err)
	}
	if err := sound.PlayOnce(context.Background()); err != nil {
		t.Fatalf("PlayOnce: %v", err)
	}
	if player.playCount() != 1 {
		t.Fatalf("played %d times, want 1", player.playCount())
	}

	// The successful load is cached: a further trigger does not re-decode.
	if _, err := loader.Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if decodes.Load() != 1 {
		t.Fatalf("decoded %d times, want the successful decode cached", decodes.Load())
	}
}

func TestBeepLoaderRetriesAfterDecodeAndDeviceFailures(t *testing.T) {
	player := &fakePlayer{}
	decodeErr := errors.New("undecodable audio")
	loader, decodes := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, decodeErr)

	if _, err := loader.Load(); err == nil {
		t.Fatalf("Load succeeded on undecodable audio")
	}

	// Replace the decoder with a working one: the failure must not be cached.
	loader.decode = func(string) (*beep.Buffer, error) {
		buffer := beep.NewBuffer(beep.Format{SampleRate: beep.SampleRate(44100), NumChannels: 2, Precision: 2})
		buffer.Append(beep.Silence(100))
		return buffer, nil
	}
	player.initErr = errors.New("audio device busy")
	if _, err := loader.Load(); err == nil {
		t.Fatalf("Load succeeded while the audio device was unavailable")
	}

	player.initErr = nil
	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load after the device recovered: %v", err)
	}
	if err := sound.PlayOnce(context.Background()); err != nil {
		t.Fatalf("PlayOnce: %v", err)
	}
	if decodes.Load() != 1 {
		t.Fatalf("decoder called %d times, want 1 (the first call failed before decoding)", decodes.Load())
	}
}

func TestBeepLoaderCloseIsSafeBeforeLoadAndIdempotent(t *testing.T) {
	player := &fakePlayer{}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)

	if err := loader.Close(); err != nil {
		t.Fatalf("Close before Load = %v", err)
	}
	if _, err := loader.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if player.closed.Load() != 1 {
		t.Fatalf("player closed %d times, want 1", player.closed.Load())
	}
}

// TestBeepSoundCancelsMidPlayback proves cancellation does not wait for the
// chime to finish on its own.
func TestBeepSoundCancelsMidPlayback(t *testing.T) {
	player := &fakePlayer{holdBlocks: true}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)
	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sound.PlayOnce(ctx) }()

	waitUntil(t, "playback to start", func() bool { return player.playCount() == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PlayOnce error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("PlayOnce did not return after cancellation")
	}
}

func TestBeepSoundReportsPlaybackErrors(t *testing.T) {
	player := &fakePlayer{playErr: errors.New("device lost")}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)
	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := sound.PlayOnce(context.Background()); err == nil {
		t.Fatalf("PlayOnce succeeded although the player failed")
	}
}

// TestBeepLoaderRecoversAfterPlaybackFailure covers a device that disappears
// mid-run: the failed playback discards the cached device, so the next trigger
// reopens it rather than staying broken.
func TestBeepLoaderRecoversAfterPlaybackFailure(t *testing.T) {
	player := &fakePlayer{playErr: errors.New("device lost")}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)

	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := sound.PlayOnce(context.Background()); err == nil {
		t.Fatalf("PlayOnce succeeded although the player failed")
	}
	if player.closed.Load() != 1 {
		t.Fatalf("failed device closed %d times, want 1", player.closed.Load())
	}

	// The device works again; the next Load must not reuse the dead one.
	player.playErr = nil
	recovered, err := loader.Load()
	if err != nil {
		t.Fatalf("Load after playback failure: %v", err)
	}
	if err := recovered.PlayOnce(context.Background()); err != nil {
		t.Fatalf("PlayOnce after recovery: %v", err)
	}
	if got := player.playCount(); got != 2 {
		t.Fatalf("player used %d times, want 2", got)
	}
}

// TestCancelDoesNotInvalidateAudio checks that an ordinary quit (cancellation)
// does not mark the device as broken.
func TestCancelDoesNotInvalidateAudio(t *testing.T) {
	player := &fakePlayer{holdBlocks: true}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)

	sound, err := loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sound.PlayOnce(ctx) }()
	waitUntil(t, "playback to start", func() bool { return player.playCount() == 1 })
	cancel()
	<-done

	loader.mu.Lock()
	cached := loader.output
	loader.mu.Unlock()
	if cached == nil {
		t.Fatalf("cancellation discarded the audio device")
	}
	if player.closed.Load() != 0 {
		t.Fatalf("device closed %d times on cancellation, want 0", player.closed.Load())
	}
}

// TestSchedulerDrivesInjectedLoader runs the real scheduler against the real
// beepLoader with a fake device, covering the wiring used by main.
func TestSchedulerDrivesInjectedLoader(t *testing.T) {
	player := &fakePlayer{}
	loader, _ := newTestLoader(func() (string, error) { return "chime.mp3", nil }, player, nil)

	var reported atomic.Int32
	s := chime.New(chime.Options{
		Loader:   loader,
		Reporter: chime.ReporterFunc(func(error) { reported.Add(1) }),
		Gap:      -1,
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A quarter hour plays exactly one chime.
	if !s.Trigger(time.Date(2026, 3, 1, 9, 45, 0, 0, time.UTC)) {
		t.Fatalf("trigger was not accepted")
	}
	waitUntil(t, "one chime to play", func() bool { return player.playCount() == 1 })

	// On the hour it plays one chime per hour count: 15:00 -> three chimes.
	if !s.Trigger(time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("trigger at 15:00 was not accepted")
	}
	waitUntil(t, "three chimes for 15:00", func() bool { return player.playCount() == 4 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if player.closed.Load() != 1 {
		t.Fatalf("player closed %d times, want 1", player.closed.Load())
	}
	if reported.Load() != 0 {
		t.Fatalf("%d failures reported, want none", reported.Load())
	}
}

func waitUntil(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !fn() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}
