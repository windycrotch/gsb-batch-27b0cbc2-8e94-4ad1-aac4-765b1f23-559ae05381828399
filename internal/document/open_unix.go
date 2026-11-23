//go:build !windows

package document

import (
	"os"

	"golang.org/x/sys/unix"
)

// openNonblocking opens without blocking on FIFOs; the regular-file check in
// OpenRegular runs on the resulting descriptor.
func openNonblocking(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
