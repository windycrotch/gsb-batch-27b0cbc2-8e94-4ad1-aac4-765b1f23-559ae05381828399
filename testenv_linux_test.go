//go:build !windows && !darwin

package volume

import (
	"os"
	"os/exec"
)

// useIsolatedFakePactl redirects the testdata/pactl fake backend at a
// private state directory for this test process. Without this, the fake's
// default /tmp state files are shared by every test binary, which makes
// running several packages in one "go test ./..." nondeterministic. It is
// a no-op when the fake backend is not on PATH (for example on a machine
// using a real PulseAudio or the amixer backend).
func useIsolatedFakePactl() {
	if path, err := exec.LookPath("pactl"); err == nil && path != "" {
		dir, err := os.MkdirTemp("", "pactl-fake-state-")
		if err == nil {
			_ = os.Setenv("PACTL_FAKE_STATE", dir)
		}
	}
}
