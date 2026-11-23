//go:build !windows
// +build !windows

package capture

import (
	"errors"

	"github.com/moutend/go-wca/pkg/wca"
)

// NewBufferSource is unavailable on non-Windows platforms. The capture session
// itself is platform independent and is tested with fake buffer sources.
func NewBufferSource(client *wca.IAudioCaptureClient, blockAlign uint16) (BufferSource, error) {
	return nil, errors.New("capture: audio capture clients are only supported on Windows")
}
