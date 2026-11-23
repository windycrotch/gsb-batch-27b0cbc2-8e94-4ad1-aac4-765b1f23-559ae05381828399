package handshake

import (
	"errors"
	"fmt"
	"io"
)

var (
	// ErrPeerInvalidData is returned (wrapped) when the peer sends data
	// that fails validation (unsupported version, invalid digest,
	// invalid Diffie-Hellman public key, non-matching echo data).
	ErrPeerInvalidData = errors.New("peer sent invalid handshake data")

	// ErrPeerClosed is returned (wrapped) when the peer closes the
	// connection before the handshake is complete.
	ErrPeerClosed = errors.New("peer closed the connection during handshake")

	// ErrRandomSource is returned (wrapped) when the local random
	// source fails. When this error is returned, no byte has been
	// written to the connection yet.
	ErrRandomSource = errors.New("random source failed")
)

// classifyReadError labels read errors caused by a peer that
// disconnected prematurely.
func classifyReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %w", ErrPeerClosed, err)
	}
	return err
}

// invalidDataError labels errors caused by invalid data received from the peer.
func invalidDataError(err error) error {
	return fmt.Errorf("%w: %w", ErrPeerInvalidData, err)
}
