package rc4rw_test

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib/pkg/rc4rw"
)

var _ io.ReadWriter = (*rc4rw.ReadWriter)(nil)

var (
	testKey1 = []byte("0123456789abcdef")
	testKey2 = []byte("fedcba9876543210")
)

func TestReadWriterRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()  //nolint:errcheck
	defer c2.Close()  //nolint:errcheck

	// the two ends of the connection must be initialized with swapped keys.
	rw1, err := rc4rw.New(c1, testKey1, testKey2)
	require.NoError(t, err)

	rw2, err := rc4rw.New(c2, testKey2, testKey1)
	require.NoError(t, err)

	msg1 := []byte("first message")
	msg2 := []byte("second message, a bit longer")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err2 := rw1.Write(msg1)
		require.NoError(t, err2)
		_, err2 = rw1.Write(msg2)
		require.NoError(t, err2)
	}()

	// the read key stream must stay synchronized across multiple reads.
	buf := make([]byte, len(msg1)+len(msg2))
	_, err = io.ReadFull(rw2, buf)
	require.NoError(t, err)
	require.Equal(t, msg1, buf[:len(msg1)])
	require.Equal(t, msg2, buf[len(msg1):])
	wg.Wait()

	// reverse direction
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err2 := rw2.Write(msg2)
		require.NoError(t, err2)
	}()

	buf = make([]byte, len(msg2))
	_, err = io.ReadFull(rw1, buf)
	require.NoError(t, err)
	require.Equal(t, msg2, buf)
	wg.Wait()
}

type bufferReadWriter struct {
	io.Reader
	io.Writer
}

func TestReadWriterWriteDoesNotMutateInput(t *testing.T) {
	var out bytes.Buffer

	rw, err := rc4rw.New(&bufferReadWriter{
		Reader: bytes.NewReader(nil),
		Writer: &out,
	}, testKey1, testKey2)
	require.NoError(t, err)

	msg := []byte("some plaintext data")
	orig := append([]byte(nil), msg...)

	_, err = rw.Write(msg)
	require.NoError(t, err)

	// the input buffer must not be modified.
	require.Equal(t, orig, msg)

	// the data on the wire must be encrypted.
	require.NotEqual(t, msg, out.Bytes())

	// a reader initialized with swapped keys must recover the plaintext.
	rw2, err := rc4rw.New(&bufferReadWriter{
		Reader: bytes.NewReader(out.Bytes()),
		Writer: io.Discard,
	}, testKey2, testKey1)
	require.NoError(t, err)

	dec := make([]byte, len(orig))
	_, err = io.ReadFull(rw2, dec)
	require.NoError(t, err)
	require.Equal(t, orig, dec)
}

func TestReadWriterInvalidKey(t *testing.T) {
	_, err := rc4rw.New(&bufferReadWriter{
		Reader: bytes.NewReader(nil),
		Writer: io.Discard,
	}, nil, testKey2)
	require.Error(t, err)

	_, err = rc4rw.New(&bufferReadWriter{
		Reader: bytes.NewReader(nil),
		Writer: io.Discard,
	}, testKey1, nil)
	require.Error(t, err)
}
