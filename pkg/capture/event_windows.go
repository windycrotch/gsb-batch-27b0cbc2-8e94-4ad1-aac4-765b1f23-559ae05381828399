//go:build windows
// +build windows

package capture

import (
	"context"
	"fmt"

	"github.com/moutend/go-wca/pkg/wca"
)

const (
	waitObject0 = 0x00000000
	waitTimeout = 0x00000102
)

// eventPollInterval is the longest time a single WaitForSingleObject call
// blocks before the capture loop checks the context again.
//
// Waiting with a finite timeout instead of INFINITE is deliberate. A
// WaitForSingleObject call blocked with INFINITE cannot be interrupted by a
// context, and a waiting goroutine would be leaked for every canceled wait.
// Here the wait always returns, so exactly one waiter at a time observes the
// event handle and waiters cannot accumulate over a long recording. The audio
// ready event keeps its signaled state until it is consumed, so no
// notification is lost when the timeout expires.
const eventPollInterval = 50 // milliseconds

type windowsEventSource struct {
	event uintptr
}

// NewEventSource wraps the audio ready event handle used in event driven mode.
//
// The returned EventSource does not own event; the caller remains responsible
// for closing the handle after the capture session has been cleaned up.
func NewEventSource(event uintptr) (EventSource, error) {
	if event == 0 {
		return nil, fmt.Errorf("capture: invalid audio event handle")
	}
	return &windowsEventSource{event: event}, nil
}

func (s *windowsEventSource) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch dw := wca.WaitForSingleObject(s.event, eventPollInterval); dw {
		case waitObject0:
			return nil
		case waitTimeout:
			// Check for cancellation and wait again.
			continue
		default:
			return fmt.Errorf("capture: failed to wait for audio event (result %#x)", dw)
		}
	}
}

func (s *windowsEventSource) Close() error {
	// The event handle is owned by the caller.
	return nil
}
