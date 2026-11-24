package volume

import (
	"errors"
	"sync"
	"testing"
)

// fakeBackend is an in-memory audioBackend used for deterministic tests.
// It records its own calls with a mutex so that the race detector can
// verify the controller serializes access, and any of its operations can
// be made to fail independently.
type fakeBackend struct {
	mu      sync.Mutex
	volume  int
	muted   bool
	statusN int
	setVolN int
	setMutN int

	failStatus  error
	failSetVol  error
	failSetMute error
}

func (f *fakeBackend) Status() (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusN++
	if f.failStatus != nil {
		return State{}, f.failStatus
	}
	return State{Volume: f.volume, Muted: f.muted}, nil
}

func (f *fakeBackend) SetVolume(volume int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setVolN++
	if f.failSetVol != nil {
		return f.failSetVol
	}
	f.volume = volume
	return nil
}

func (f *fakeBackend) SetMuted(muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setMutN++
	if f.failSetMute != nil {
		return f.failSetMute
	}
	f.muted = muted
	return nil
}

func (f *fakeBackend) snapshotState() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return State{Volume: f.volume, Muted: f.muted}
}

func (f *fakeBackend) callCount() (status, setVol, setMute int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusN, f.setVolN, f.setMutN
}

// withFakeBackend installs fb as the package backend for the duration of
// the test. Tests using it never run in parallel with each other.
func withFakeBackend(t *testing.T, fb *fakeBackend) {
	t.Helper()
	restore := swapBackend(fb)
	t.Cleanup(restore)
}

var errInjected = errors.New("injected backend failure")

// checkAudioControl verifies not only that the audio state can be read but
// also that it can be changed: some virtualized endpoints accept reads yet
// reject every write (for example with ERROR_INVALID_FUNCTION). Hardware
// tests are only meaningful when writes work too. The original volume and
// muted state are restored by the probe.
func checkAudioControl() bool {
	volume, err := GetVolume()
	if err != nil {
		return false
	}
	muted, err := GetMuted()
	if err != nil {
		return false
	}
	if err := SetVolume(volume); err != nil {
		return false
	}
	if err := Unmute(); err != nil {
		return false
	}
	if muted {
		if err := Mute(); err != nil {
			return false
		}
	}
	return true
}

// TestAudioControlProbe runs before the hardware tests and records whether
// the host provides a fully writable audio endpoint. It never fails: a
// negative result simply makes the hardware-dependent tests skip.
func TestAudioControlProbe(t *testing.T) {
	if !audioAvailable {
		t.Skip("no writable audio device available; hardware-dependent tests will skip")
	}
}

// TestConcurrentIncrements verifies that many increment commands executed
// concurrently converge to exactly the initial volume plus the sum of the
// increments: no increment is lost or applied twice.
func TestConcurrentIncrements(t *testing.T) {
	const initial, delta, goroutines = 10, 1, 64
	fb := &fakeBackend{volume: initial}
	withFakeBackend(t, fb)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := IncreaseVolume(delta); err != nil {
				t.Errorf("IncreaseVolume failed: %+v", err)
			}
		}()
	}
	wg.Wait()

	want := initial + delta*goroutines
	if got := fb.snapshotState().Volume; got != want {
		t.Errorf("concurrent increments failed: (got: %d, expected: %d)", got, want)
	}
	got, err := GetVolume()
	if err != nil {
		t.Fatalf("GetVolume failed: %+v", err)
	}
	if got != want {
		t.Errorf("concurrent increments failed: (got: %d, expected: %d)", got, want)
	}
}

// TestConcurrentUpAndDown reproduces two terminals issuing up/down at the
// same time and checks that positive and negative increments all apply.
func TestConcurrentUpAndDown(t *testing.T) {
	const initial, ups, downs, step = 40, 30, 30, 2
	fb := &fakeBackend{volume: initial}
	withFakeBackend(t, fb)

	var wg sync.WaitGroup
	for i := 0; i < ups; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := IncreaseVolume(step); err != nil {
				t.Errorf("IncreaseVolume up failed: %+v", err)
			}
		}()
	}
	for i := 0; i < downs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := IncreaseVolume(-step); err != nil {
				t.Errorf("IncreaseVolume down failed: %+v", err)
			}
		}()
	}
	wg.Wait()

	if got := fb.snapshotState().Volume; got != initial {
		t.Errorf("concurrent up/down failed: (got: %d, expected: %d)", got, initial)
	}
}

// TestConcurrentIncrementsClamped verifies that when the sum reaches the
// boundary, every command still succeeds and the result is the truncated
// value (100), not a lost or duplicated increment.
func TestConcurrentIncrementsClamped(t *testing.T) {
	const initial, delta, goroutines = 95, 5, 20
	fb := &fakeBackend{volume: initial}
	withFakeBackend(t, fb)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := IncreaseVolume(delta); err != nil {
				t.Errorf("IncreaseVolume failed: %+v", err)
			}
		}()
	}
	wg.Wait()

	if got := fb.snapshotState().Volume; got != 100 {
		t.Errorf("clamped concurrent increments failed: (got: %d, expected: 100)", got)
	}
	// All goroutines must have completed a full read-modify-write cycle.
	_, setVol, setMute := fb.callCount()
	if setVol != goroutines || setMute != goroutines {
		t.Errorf("every increment must be applied once: (setVol: %d, setMute: %d, expected: %d)", setVol, setMute, goroutines)
	}
}

// TestAdjustmentClearsMute verifies that every adjustment command
// (set/up/down) performed while muted clears the muted flag and keeps the
// volume inside the 0-100 range.
func TestAdjustmentClearsMute(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
		want int
	}{
		{"set", func() error { return SetVolume(42) }, 42},
		{"up", func() error { return IncreaseVolume(5) }, 35},
		{"down", func() error { return IncreaseVolume(-5) }, 25},
		{"set-zero", func() error { return SetVolume(0) }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{volume: 30, muted: true}
			withFakeBackend(t, fb)
			if err := tc.run(); err != nil {
				t.Fatalf("%s failed: %+v", tc.name, err)
			}
			state := fb.snapshotState()
			if state.Muted {
				t.Errorf("%s left the audio muted", tc.name)
			}
			if state.Volume != tc.want {
				t.Errorf("%s volume failed: (got: %d, expected: %d)", tc.name, state.Volume, tc.want)
			}
			muted, err := GetMuted()
			if err != nil {
				t.Fatalf("GetMuted failed: %+v", err)
			}
			if muted {
				t.Errorf("%s: GetMuted still reports muted", tc.name)
			}
		})
	}
}

// TestIncreaseVolumeClampsRange checks that both overflow and underflow
// are clamped into 0-100 and that the mute flag is cleared.
func TestIncreaseVolumeClampsRange(t *testing.T) {
	fb := &fakeBackend{volume: 90, muted: true}
	withFakeBackend(t, fb)
	if err := IncreaseVolume(1000); err != nil {
		t.Fatalf("IncreaseVolume failed: %+v", err)
	}
	if state := fb.snapshotState(); state.Volume != 100 || state.Muted {
		t.Errorf("upper clamp failed: (got: %+v, expected volume 100 and unmuted)", state)
	}

	fb.volume, fb.muted = 5, true
	if err := IncreaseVolume(-1000); err != nil {
		t.Fatalf("IncreaseVolume failed: %+v", err)
	}
	if state := fb.snapshotState(); state.Volume != 0 || state.Muted {
		t.Errorf("lower clamp failed: (got: %+v, expected volume 0 and unmuted)", state)
	}
}

// TestSetVolumeRejectsOutOfRange verifies that invalid values are rejected
// before touching the backend, leaving the real state untouched.
func TestSetVolumeRejectsOutOfRange(t *testing.T) {
	for _, volume := range []int{-1, 101, 1000} {
		fb := &fakeBackend{volume: 50, muted: true}
		withFakeBackend(t, fb)
		if err := SetVolume(volume); err == nil {
			t.Errorf("SetVolume(%d) should fail", volume)
		}
		if state := fb.snapshotState(); state.Volume != 50 || !state.Muted {
			t.Errorf("failed SetVolume must not change state: (got: %+v)", state)
		}
		if _, setVol, setMute := fb.callCount(); setVol != 0 || setMute != 0 {
			t.Errorf("failed SetVolume must not call the backend: (setVol: %d, setMute: %d)", setVol, setMute)
		}
	}
}

// TestBackendFailureNoCachedState verifies that a failing backend command
// returns an error, caches nothing, and that the next status read goes
// back to the real backend instead of reporting a stale value.
func TestBackendFailureNoCachedState(t *testing.T) {
	fb := &fakeBackend{volume: 33, muted: false}
	withFakeBackend(t, fb)

	// A failed read must surface an error and not invent a value.
	fb.failStatus = errInjected
	if _, err := GetStatus(); !errors.Is(err, errInjected) {
		t.Fatalf("GetStatus error failed: (got: %+v, expected: %+v)", err, errInjected)
	}
	if _, err := GetVolume(); !errors.Is(err, errInjected) {
		t.Fatalf("GetVolume should propagate the read failure: (got: %+v)", err)
	}
	if _, err := GetMuted(); !errors.Is(err, errInjected) {
		t.Fatalf("GetMuted should propagate the read failure: (got: %+v)", err)
	}

	// Once the backend recovers, status must report the real state again:
	// nothing was cached during the failure.
	fb.failStatus = nil
	state, err := GetStatus()
	if err != nil {
		t.Fatalf("GetStatus after recovery failed: %+v", err)
	}
	if state.Volume != 33 || state.Muted {
		t.Errorf("stale or wrong state after recovery: (got: %+v)", state)
	}
}

// TestAdjustmentFailureRecovery verifies that a failure of the volume
// write or the unmute write returns non-zero, leaves no cached success,
// and the following legal command works normally against the real backend.
func TestAdjustmentFailureRecovery(t *testing.T) {
	t.Run("set-volume-fails", func(t *testing.T) {
		fb := &fakeBackend{volume: 30, muted: true}
		withFakeBackend(t, fb)
		fb.failSetVol = errInjected
		if err := SetVolume(60); !errors.Is(err, errInjected) {
			t.Fatalf("SetVolume should fail: (got: %+v, expected: %+v)", err, errInjected)
		}
		if state := fb.snapshotState(); state.Volume != 30 || !state.Muted {
			t.Errorf("failed adjustment must leave real state untouched: (got: %+v)", state)
		}
		fb.failSetVol = nil
		if err := SetVolume(60); err != nil {
			t.Fatalf("SetVolume after recovery failed: %+v", err)
		}
		if state := fb.snapshotState(); state.Volume != 60 || state.Muted {
			t.Errorf("state after recovery failed: (got: %+v)", state)
		}
	})

	t.Run("unmute-fails", func(t *testing.T) {
		fb := &fakeBackend{volume: 30, muted: true}
		withFakeBackend(t, fb)
		fb.failSetMute = errInjected
		if err := IncreaseVolume(10); !errors.Is(err, errInjected) {
			t.Fatalf("IncreaseVolume should propagate the unmute failure: (got: %+v)", err)
		}
		// No success must be cached: the next status reflects the real
		// backend state.
		fb.failSetMute = nil
		state, err := GetStatus()
		if err != nil {
			t.Fatalf("GetStatus after failure failed: %+v", err)
		}
		if state.Muted != true {
			t.Errorf("GetStatus must report the real muted state, not a cached unmute")
		}
		// A subsequent legal command still works and clears the mute.
		if err := IncreaseVolume(10); err != nil {
			t.Fatalf("IncreaseVolume after recovery failed: %+v", err)
		}
		if state := fb.snapshotState(); state.Volume != 50 || state.Muted {
			t.Errorf("state after recovery failed: (got: %+v)", state)
		}
	})

	t.Run("mute-commands-fail", func(t *testing.T) {
		fb := &fakeBackend{volume: 20, muted: false}
		withFakeBackend(t, fb)
		fb.failSetMute = errInjected
		if err := Mute(); !errors.Is(err, errInjected) {
			t.Fatalf("Mute should fail: (got: %+v)", err)
		}
		if err := Unmute(); !errors.Is(err, errInjected) {
			t.Fatalf("Unmute should fail: (got: %+v)", err)
		}
		fb.failSetMute = nil
		if err := Mute(); err != nil {
			t.Fatalf("Mute after recovery failed: %+v", err)
		}
		if state := fb.snapshotState(); !state.Muted {
			t.Errorf("Mute after recovery failed: (got: %+v)", state)
		}
	})
}

// TestStatusSnapshotConsistent verifies that GetStatus returns volume and
// muted from one coherent read, and that every call re-reads the backend
// rather than returning a cached snapshot.
func TestStatusSnapshotConsistent(t *testing.T) {
	fb := &fakeBackend{volume: 71, muted: true}
	withFakeBackend(t, fb)

	first, err := GetStatus()
	if err != nil {
		t.Fatalf("GetStatus failed: %+v", err)
	}
	if first.Volume != 71 || !first.Muted {
		t.Errorf("snapshot failed: (got: %+v)", first)
	}

	// Change the real backend out-of-band; the next snapshot must observe
	// it, proving nothing stale is served.
	fb.mu.Lock()
	fb.volume, fb.muted = 84, false
	fb.mu.Unlock()
	second, err := GetStatus()
	if err != nil {
		t.Fatalf("GetStatus failed: %+v", err)
	}
	if second.Volume != 84 || second.Muted {
		t.Errorf("snapshot not refreshed from real state: (got: %+v)", second)
	}

	statusN, _, _ := fb.callCount()
	if statusN < 2 {
		t.Errorf("GetStatus must read the backend on every call: (reads: %d)", statusN)
	}
}

// TestFailoverBackendRecovery exercises the platform-neutral failover
// logic used on Linux: a preferred-backend failure returns an error and
// degrades to the fallback, while a recovered preferred backend is
// re-probed and picked up again, so no failure permanently pins the wrong
// backend.
func TestFailoverBackendRecovery(t *testing.T) {
	preferred := &fakeBackend{volume: 11}
	fallback := &fakeBackend{volume: 22}
	probeOK := true
	fo := &failoverBackend{
		preferred: preferred,
		fallback:  fallback,
		probe: func() error {
			if probeOK {
				return nil
			}
			return errInjected
		},
	}

	// Healthy preferred backend is used.
	if state, err := fo.Status(); err != nil || state.Volume != 11 {
		t.Fatalf("preferred backend should be used: (state: %+v, err: %+v)", state, err)
	}

	// Preferred backend fails: error must propagate and degradation occurs.
	preferred.failStatus = errInjected
	if _, err := fo.Status(); !errors.Is(err, errInjected) {
		t.Fatalf("preferred failure must surface: (got: %+v)", err)
	}

	// Probe still failing -> fallback serves subsequent calls.
	probeOK = false
	if state, err := fo.Status(); err != nil || state.Volume != 22 {
		t.Fatalf("fallback backend should serve while degraded: (state: %+v, err: %+v)", state, err)
	}
	if err := fo.SetVolume(33); err != nil {
		t.Fatalf("fallback SetVolume failed: %+v", err)
	}
	if state := fallback.snapshotState(); state.Volume != 33 {
		t.Errorf("fallback SetVolume failed: (got: %+v)", state)
	}

	// Preferred backend recovers (both probe and command): traffic returns
	// to it instead of staying pinned to the fallback.
	probeOK = true
	preferred.failStatus = nil
	if state, err := fo.Status(); err != nil || state.Volume != 11 {
		t.Fatalf("recovered preferred backend should be selected: (state: %+v, err: %+v)", state, err)
	}
}
