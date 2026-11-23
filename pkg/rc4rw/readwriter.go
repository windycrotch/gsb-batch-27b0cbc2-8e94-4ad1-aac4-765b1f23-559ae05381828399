// Package rc4rw contains an RC4 ReadWriter.
package rc4rw

import (
	"crypto/rc4"
	"io"
	"sync"
)

// ReadWriter is an RC4 ReadWriter.
//
// Reads and writes performed on the same instance share the same pair of
// key streams: reads always use keyIn, writes always use keyOut, regardless
// of the order in which they happen. The two ends of a connection must
// therefore be initialized with swapped keys. It is safe for concurrent use.
type ReadWriter struct {
	rw io.ReadWriter

	readMutex sync.Mutex
	in        *rc4.Cipher

	writeMutex sync.Mutex
	out        *rc4.Cipher
}

// New creates a new RC4 ReadWriter with the given keys.
func New(rw io.ReadWriter, keyIn []byte, keyOut []byte) (*ReadWriter, error) {
	in, err := rc4.NewCipher(keyIn)
	if err != nil {
		return nil, err
	}

	out, err := rc4.NewCipher(keyOut)
	if err != nil {
		return nil, err
	}

	p := make([]byte, 1536)
	in.XORKeyStream(p, p)
	out.XORKeyStream(p, p)

	return &ReadWriter{
		rw:  rw,
		in:  in,
		out: out,
	}, nil
}

// Read implements io.Reader.
func (r *ReadWriter) Read(p []byte) (int, error) {
	r.readMutex.Lock()
	defer r.readMutex.Unlock()

	n, err := r.rw.Read(p)
	if n == 0 {
		return 0, err
	}

	r.in.XORKeyStream(p[:n], p[:n])
	return n, err
}

// Write implements io.Writer.
func (r *ReadWriter) Write(p []byte) (int, error) {
	r.writeMutex.Lock()
	defer r.writeMutex.Unlock()

	// encrypt into a temporary buffer, since Write must not
	// modify the provided one.
	buf := make([]byte, len(p))
	r.out.XORKeyStream(buf, p)
	return r.rw.Write(buf)
}
