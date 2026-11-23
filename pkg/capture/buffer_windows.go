//go:build windows
// +build windows

package capture

import (
	"fmt"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// audClntSBufferEmpty is AUDCLNT_S_BUFFER_EMPTY. It is a success code, but the
// syscall based bindings treat any non-zero HRESULT as an error, so it is
// mapped to ErrNoData instead.
const audClntSBufferEmpty = 0x08890001

type windowsBufferSource struct {
	client     *wca.IAudioCaptureClient
	blockAlign uint16
}

// NewBufferSource adapts an IAudioCaptureClient to the BufferSource interface.
//
// blockAlign is the size of a single audio frame in bytes. The returned buffer
// source copies every captured block out of the endpoint buffer so that the
// data stays valid after Release.
func NewBufferSource(client *wca.IAudioCaptureClient, blockAlign uint16) (BufferSource, error) {
	if client == nil {
		return nil, fmt.Errorf("capture: nil audio capture client")
	}
	if blockAlign == 0 {
		return nil, fmt.Errorf("capture: block align must not be zero")
	}
	return &windowsBufferSource{client: client, blockAlign: blockAlign}, nil
}

func (s *windowsBufferSource) Acquire() (frames uint32, data []byte, err error) {
	var (
		raw             *byte
		flags           uint32
		devicePosition  uint64
		qpcPosition     uint64
		framesAvailable uint32
	)

	if err = s.client.GetBuffer(&raw, &framesAvailable, &flags, &devicePosition, &qpcPosition); err != nil {
		if oleErr, ok := err.(*ole.OleError); ok && oleErr.Code() == audClntSBufferEmpty {
			return 0, nil, ErrNoData
		}
		return 0, nil, fmt.Errorf("capture: failed to get the capture buffer: %w", err)
	}

	size := int(framesAvailable) * int(s.blockAlign)
	if size == 0 || raw == nil {
		// The endpoint can report an empty packet. It still counts as an
		// acquired buffer, so a matching Release must follow.
		return framesAvailable, nil, nil
	}

	data = copyBytes(raw, size)
	return framesAvailable, data, nil
}

func (s *windowsBufferSource) Release(frames uint32) error {
	if err := s.client.ReleaseBuffer(frames); err != nil {
		return fmt.Errorf("capture: failed to release the capture buffer: %w", err)
	}
	return nil
}

// copyBytes copies size bytes starting at data into a new slice.
func copyBytes(data *byte, size int) []byte {
	src := (*[1 << 30]byte)(unsafe.Pointer(data))[:size:size]
	buf := make([]byte, size)
	copy(buf, src)
	return buf
}
