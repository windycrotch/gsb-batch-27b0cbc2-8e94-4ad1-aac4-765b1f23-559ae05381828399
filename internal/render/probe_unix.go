//go:build !windows

package render

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether more input arrives before the deadline.
func waitReadable(f *os.File, d time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	ready, err := unix.Poll(fds, int(d.Milliseconds()))
	return err == nil && ready > 0 && fds[0].Revents&unix.POLLIN != 0
}
