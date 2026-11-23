//go:build windows

package document

import "os"

// Windows has no blocking FIFO opens on ordinary paths; a plain open followed
// by the regular-file check in OpenRegular is sufficient.
func openNonblocking(path string) (*os.File, error) { return os.Open(path) }
