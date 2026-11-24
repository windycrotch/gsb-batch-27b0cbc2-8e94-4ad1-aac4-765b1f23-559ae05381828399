package volume

import (
	"errors"
	"sync"
)

// ErrInvalidVolume is returned when a volume value outside the 0-100 range
// is passed to SetVolume.
var ErrInvalidVolume = errors.New("out of valid volume range")

// State is a consistent snapshot of the audio volume and the muted flag,
// read from the backend in a single critical section.
type State struct {
	// Volume is the current audio volume, clamped between 0 and 100.
	Volume int
	// Muted reports whether the audio is currently muted.
	Muted bool
}

// audioBackend is the platform-specific audio control surface.
//
// Every implementation must be safe for concurrent use only when calls are
// serialized by the controller's mutex; implementations themselves do not
// need to lock. None of the methods may cache values returned by the
// underlying system: each call has to observe the real state so that a
// failure can never leave stale information behind.
type audioBackend interface {
	// Status returns the current volume and muted status as a single
	// consistent snapshot.
	Status() (State, error)
	// SetVolume applies an absolute volume, guaranteed to be within 0-100.
	SetVolume(volume int) error
	// SetMuted applies the muted flag.
	SetMuted(muted bool) error
}

// controller serializes every interaction with the audio backend.
//
// Read-modify-write operations (IncreaseVolume, and the unmute that follows
// an adjustment) run inside a single lock acquisition so that concurrent
// commands cannot interleave and lose or duplicate increments. The package
// never caches state: values are always read through the backend, which
// means a failed backend call leaves nothing behind to be mistaken for a
// successful result later.
type controller struct {
	mu      sync.Mutex
	backend audioBackend
}

var defaultController = &controller{backend: newDefaultBackend()}

// swapBackend replaces the backend used by the package and returns a
// function restoring the previous one. It is intended for tests.
func swapBackend(backend audioBackend) func() {
	defaultController.mu.Lock()
	prev := defaultController.backend
	defaultController.backend = backend
	defaultController.mu.Unlock()
	return func() {
		defaultController.mu.Lock()
		defaultController.backend = prev
		defaultController.mu.Unlock()
	}
}

// GetStatus returns a consistent snapshot of the current volume and muted
// status. The value is always freshly read from the system; nothing is
// cached, so the next call re-reads the real state even after a failure.
func GetStatus() (State, error) {
	defaultController.mu.Lock()
	defer defaultController.mu.Unlock()
	return defaultController.backend.Status()
}

// GetVolume returns the current volume (0 to 100).
func GetVolume() (int, error) {
	state, err := GetStatus()
	if err != nil {
		return 0, err
	}
	return state.Volume, nil
}

// GetMuted returns the current muted status.
func GetMuted() (bool, error) {
	state, err := GetStatus()
	if err != nil {
		return false, err
	}
	return state.Muted, nil
}

// SetVolume sets the sound volume to the specified value. On success the
// muted flag is cleared, matching the behavior of every adjustment
// command. An out-of-range value is rejected before the backend is
// touched.
func SetVolume(volume int) error {
	if volume < 0 || volume > 100 {
		return ErrInvalidVolume
	}
	defaultController.mu.Lock()
	defer defaultController.mu.Unlock()
	return defaultController.apply(volume)
}

// IncreaseVolume increases (or decreases) the audio volume by the
// specified value. The current volume is read and the new absolute volume
// is written atomically, so concurrent increments are applied exactly once
// each and the result is clamped to the 0-100 range. On success the muted
// flag is cleared. A backend failure is returned to the caller without
// caching any value.
func IncreaseVolume(diff int) error {
	defaultController.mu.Lock()
	defer defaultController.mu.Unlock()
	state, err := defaultController.backend.Status()
	if err != nil {
		return err
	}
	target := clamp(state.Volume + diff)
	return defaultController.apply(target)
}

// Mute mutes the audio.
func Mute() error {
	defaultController.mu.Lock()
	defer defaultController.mu.Unlock()
	return defaultController.backend.SetMuted(true)
}

// Unmute unmutes the audio.
func Unmute() error {
	defaultController.mu.Lock()
	defer defaultController.mu.Unlock()
	return defaultController.backend.SetMuted(false)
}

// apply writes the (already validated) volume and clears the muted flag
// within one critical section. If either backend call fails, the error is
// propagated and no state is cached, so the next command re-reads the real
// state. The caller must hold c.mu.
func (c *controller) apply(volume int) error {
	if err := c.backend.SetVolume(volume); err != nil {
		return err
	}
	if err := c.backend.SetMuted(false); err != nil {
		return err
	}
	return nil
}

func clamp(volume int) int {
	if volume < 0 {
		return 0
	}
	if volume > 100 {
		return 100
	}
	return volume
}

// failoverBackend selects between a preferred and a fallback backend.
//
// All calls are serialized by the controller mutex, so the fields need no
// locking of their own. When the preferred backend fails, the error is
// returned (it must not be hidden) and the backend is marked degraded. The
// preferred backend is re-probed on every subsequent call; once the probe
// succeeds again, traffic returns to it, so a single failure can never
// cause permanent degradation to the wrong backend.
type failoverBackend struct {
	preferred audioBackend
	fallback  audioBackend
	probe     func() error
	degraded  bool
}

func (b *failoverBackend) selectBackend() audioBackend {
	if b.degraded && b.probe() == nil {
		b.degraded = false
	}
	if b.degraded {
		return b.fallback
	}
	return b.preferred
}

// dispatch runs fn against the currently selected backend and records a
// failure of the preferred backend as degradation.
func (b *failoverBackend) dispatch(fn func(audioBackend) error) error {
	backend := b.selectBackend()
	if err := fn(backend); err != nil {
		if backend == b.preferred {
			b.degraded = true
		}
		return err
	}
	return nil
}

func (b *failoverBackend) Status() (state State, err error) {
	err = b.dispatch(func(backend audioBackend) error {
		state, err = backend.Status()
		return err
	})
	return state, err
}

func (b *failoverBackend) SetVolume(volume int) error {
	return b.dispatch(func(backend audioBackend) error {
		return backend.SetVolume(volume)
	})
}

func (b *failoverBackend) SetMuted(muted bool) error {
	return b.dispatch(func(backend audioBackend) error {
		return backend.SetMuted(muted)
	})
}
