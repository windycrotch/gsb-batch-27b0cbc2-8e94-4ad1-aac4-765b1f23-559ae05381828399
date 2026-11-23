//go:build !windows
// +build !windows

package capture

import (
	"errors"
)

// NewEventSource is unavailable on non-Windows platforms. The capture session
// itself is platform independent and is tested with fake event sources.
func NewEventSource(event uintptr) (EventSource, error) {
	return nil, errors.New("capture: audio event sources are only supported on Windows")
}
