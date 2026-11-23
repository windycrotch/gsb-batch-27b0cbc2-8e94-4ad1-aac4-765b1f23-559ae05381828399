//go:build windows

package render

import (
	"os"
	"time"
)

// Windows console handles cannot be polled here; a fragmented probe reply is
// reassembled by later reads instead of blocking the input loop.
func waitReadable(f *os.File, d time.Duration) bool { return false }
