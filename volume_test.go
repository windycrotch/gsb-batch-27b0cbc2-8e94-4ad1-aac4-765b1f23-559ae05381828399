package volume

import (
	"fmt"
	"math"
	"os"
	"testing"
)

// audioAvailable reports whether the machine exposes a usable, writable
// audio endpoint. Hardware-dependent tests skip when it is unavailable (for
// example a CI sandbox without an audio device or with a read-only virtual
// endpoint); the concurrency and failure-handling guarantees are covered by
// the fake-backend tests which run everywhere.
var audioAvailable bool

func TestMain(m *testing.M) {
	useIsolatedFakePactl()
	audioAvailable = checkAudioControl()
	if !audioAvailable {
		fmt.Fprintln(os.Stderr, "no writable audio device available, skipping hardware tests")
	}
	code := m.Run()
	os.Exit(code)
}

func skipIfNoAudio(t *testing.T) {
	t.Helper()
	if !audioAvailable {
		t.Skip("no audio device available; skipping hardware-dependent test")
	}
}

func TestGetVolume(t *testing.T) {
	skipIfNoAudio(t)
	_, err := GetVolume()
	if err != nil {
		t.Errorf("get volume failed: %+v", err)
	}
}

func TestSetVolume(t *testing.T) {
	skipIfNoAudio(t)
	for _, vol := range []int{0, 37, 54, 20, 10} {
		err := SetVolume(vol)
		if err != nil {
			t.Errorf("set volume failed: %+v", err)
		}
		v, err := GetVolume()
		if err != nil {
			t.Errorf("get volume failed: %+v", err)
		}
		if vol != v {
			if math.Abs(float64(v-vol)) > 4 {
				t.Errorf("set volume failed: (got: %+v, expected: %+v)", v, vol)
			} else {
				t.Logf("set volume difference (possibly amixer on Linux): (got: %+v, expected: %+v)", v, vol)
			}
		}
	}
}

func TestIncreaseVolume(t *testing.T) {
	skipIfNoAudio(t)
	vol := 17
	diff := 3
	err := SetVolume(vol)
	if err != nil {
		t.Errorf("set volume failed: %+v", err)
	}
	err = IncreaseVolume(diff)
	if err != nil {
		t.Errorf("increase volume failed: %+v", err)
	}
	v, err := GetVolume()
	if err != nil {
		t.Errorf("get volume failed: %+v", err)
	}
	if v != vol+diff {
		if vol := vol + diff; math.Abs(float64(v-vol)) > 4 {
			t.Errorf("increase volume failed: (got: %+v, expected: %+v)", v, vol)
		} else {
			t.Logf("increase volume difference (possibly amixer on Linux): (got: %+v, expected: %+v)", v, vol)
		}
	}
	err = IncreaseVolume(-diff)
	if err != nil {
		t.Errorf("increase volume failed: %+v", err)
	}
	v, err = GetVolume()
	if err != nil {
		t.Errorf("get volume failed: %+v", err)
	}
	if v != vol {
		if math.Abs(float64(v-vol)) > 4 {
			t.Errorf("increase volume failed: (got: %+v, expected: %+v)", v, vol)
		} else {
			t.Logf("increase volume difference (possibly amixer on Linux): (got: %+v, expected: %+v)", v, vol)
		}
	}
	err = IncreaseVolume(-100)
	if err != nil {
		t.Errorf("increase volume failed: %+v", err)
	}
	v, err = GetVolume()
	if err != nil {
		t.Errorf("get volume failed: %+v", err)
	}
	if v != 0 {
		t.Errorf("increase volume failed: (got: %+v, expected: %+v)", v, 0)
	}
}

func TestMute(t *testing.T) {
	skipIfNoAudio(t)
	err := Mute()
	if err != nil {
		t.Errorf("mute failed: %+v", err)
	}
	muted, err := GetMuted()
	if err != nil {
		t.Errorf("get muted failed: %+v", err)
	}
	if !muted {
		t.Errorf("mute failed: %t", muted)
	}
}

func TestUnmute(t *testing.T) {
	skipIfNoAudio(t)
	err := Unmute()
	if err != nil {
		t.Errorf("unmute failed: %+v", err)
	}
	muted, err := GetMuted()
	if err != nil {
		t.Errorf("get muted failed: %+v", err)
	}
	if muted {
		t.Errorf("unmute failed: %t", muted)
	}
}
