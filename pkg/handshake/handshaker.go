package handshake

import (
	"crypto/rand"
	"io"
	"sync"
)

// Handshaker performs a single client-side or server-side handshake over a connection.
//
// The handshake is performed at most once, even in case of concurrent or
// repeated calls to Do: after the first attempt, its result (either keys or
// error) is returned as-is, and no additional byte is read from or written
// to the connection. This makes Handshaker safe for concurrent use.
type Handshaker struct {
	rw        io.ReadWriter
	isServer  bool
	encrypted bool
	strict    bool
	randRead  func([]byte) (int, error)

	mu        sync.Mutex
	performed bool
	keyIn     []byte
	keyOut    []byte
	err       error
}

// NewClientHandshaker allocates a client-side Handshaker.
func NewClientHandshaker(rw io.ReadWriter, encrypted bool, strict bool) *Handshaker {
	return NewClientHandshakerWithRand(rw, encrypted, strict, rand.Read)
}

// NewClientHandshakerWithRand allocates a client-side Handshaker, using the
// given function to generate random data.
func NewClientHandshakerWithRand(
	rw io.ReadWriter,
	encrypted bool,
	strict bool,
	randRead func([]byte) (int, error),
) *Handshaker {
	return &Handshaker{
		rw:        rw,
		encrypted: encrypted,
		strict:    strict,
		randRead:  randRead,
	}
}

// NewServerHandshaker allocates a server-side Handshaker.
func NewServerHandshaker(rw io.ReadWriter, strict bool) *Handshaker {
	return NewServerHandshakerWithRand(rw, strict, rand.Read)
}

// NewServerHandshakerWithRand allocates a server-side Handshaker, using the
// given function to generate random data.
func NewServerHandshakerWithRand(
	rw io.ReadWriter,
	strict bool,
	randRead func([]byte) (int, error),
) *Handshaker {
	return &Handshaker{
		rw:       rw,
		isServer: true,
		strict:   strict,
		randRead: randRead,
	}
}

// Do performs the handshake. If the handshake has already been performed,
// it returns the result of the first attempt without touching the connection.
func (h *Handshaker) Do() ([]byte, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.performed {
		h.performed = true
		if h.isServer {
			h.keyIn, h.keyOut, h.err = DoServerWithRand(h.rw, h.strict, h.randRead)
		} else {
			h.keyIn, h.keyOut, h.err = DoClientWithRand(h.rw, h.encrypted, h.strict, h.randRead)
		}
	}

	return h.keyIn, h.keyOut, h.err
}
