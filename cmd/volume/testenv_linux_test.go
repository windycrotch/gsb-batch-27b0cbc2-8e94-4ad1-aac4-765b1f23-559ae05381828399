//go:build !windows && !darwin

package main

import (
	"os"
	"os/exec"
)

// useIsolatedFakePactl gives this test process a private state directory
// for the testdata/pactl fake backend, so it never shares state with the
// library package's test process during a single "go test ./...".
func useIsolatedFakePactl() {
	if path, err := exec.LookPath("pactl"); err == nil && path != "" {
		if dir, err := os.MkdirTemp("", "pactl-fake-state-cmd-"); err == nil {
			_ = os.Setenv("PACTL_FAKE_STATE", dir)
		}
	}
}
