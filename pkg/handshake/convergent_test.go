package handshake_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib/pkg/handshake"
)

var errRandSource = errors.New("rand source broken")

func failingRandRead(p []byte) (int, error) {
	return 0, errRandSource
}

func TestHandshakeRandomSourceFailure(t *testing.T) {
	t.Run("client plain", func(t *testing.T) {
		rw := newSplitReadWriter(nil)
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, false, true, failingRandRead)
		require.ErrorIs(t, err, handshake.ErrRandomSource)
		require.ErrorIs(t, err, errRandSource)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("client encrypted", func(t *testing.T) {
		rw := newSplitReadWriter(nil)
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, true, true, failingRandRead)
		require.ErrorIs(t, err, handshake.ErrRandomSource)
		require.ErrorIs(t, err, errRandSource)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("server plain", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{3})
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, failingRandRead)
		require.ErrorIs(t, err, handshake.ErrRandomSource)
		require.ErrorIs(t, err, errRandSource)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("server encrypted", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{6})
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, failingRandRead)
		require.ErrorIs(t, err, handshake.ErrRandomSource)
		require.ErrorIs(t, err, errRandSource)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})
}

func TestHandshakePeerClosed(t *testing.T) {
	t.Run("client plain, immediate EOF", func(t *testing.T) {
		rw := newSplitReadWriter(nil)
		_, _, err := handshake.DoClientWithRand(rw, false, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
	})

	t.Run("client encrypted, immediate EOF", func(t *testing.T) {
		rw := newSplitReadWriter(nil)
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, true, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
	})

	t.Run("client encrypted, EOF after S0", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{6})
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, true, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
	})

	t.Run("client plain, EOF in the middle of S1", func(t *testing.T) {
		rw := newSplitReadWriter(append([]byte{3}, bytes.Repeat([]byte{0xAA}, 100)...))
		_, _, err := handshake.DoClientWithRand(rw, false, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("server, immediate EOF", func(t *testing.T) {
		rw := newSplitReadWriter(nil)
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("server, EOF after C0", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{3})
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("server, EOF in the middle of C1", func(t *testing.T) {
		rw := newSplitReadWriter(append([]byte{6}, bytes.Repeat([]byte{0xAA}, 100)...))
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerClosed)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})
}

func TestHandshakeInvalidVersion(t *testing.T) {
	t.Run("server, unsupported version", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{9})
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("client plain, unexpected version", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{6})
		_, _, err := handshake.DoClientWithRand(rw, false, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		// only C0 and C1 must have been written.
		require.Equal(t, 1537, rw.writer.Len())
	})

	t.Run("client encrypted, unexpected version", func(t *testing.T) {
		rw := newSplitReadWriter([]byte{3})
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, true, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		require.Equal(t, 1537, rw.writer.Len())
	})
}

func TestHandshakeInvalidDigest(t *testing.T) {
	t.Run("client encrypted, invalid S1 digest", func(t *testing.T) {
		input := append([]byte{6}, bytes.Repeat([]byte{0xAA}, 1536)...)
		rw := newSplitReadWriter(input)
		keyIn, keyOut, err := handshake.DoClientWithRand(rw, true, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		// C2 must not be written after the failure.
		require.Equal(t, 1537, rw.writer.Len())
	})

	t.Run("server encrypted, invalid C1 digest", func(t *testing.T) {
		input := append([]byte{6}, bytes.Repeat([]byte{0xAA}, 1536)...)
		rw := newSplitReadWriter(input)
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
		// nothing must be written after the failure.
		require.Equal(t, 0, rw.writer.Len())
	})

	t.Run("client plain, S2 does not correspond", func(t *testing.T) {
		input := append([]byte{3}, bytes.Repeat([]byte{0xAA}, 1536+1536)...)
		rw := newSplitReadWriter(input)
		_, _, err := handshake.DoClientWithRand(rw, false, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		// C2 must not be written after the failure.
		require.Equal(t, 1537, rw.writer.Len())
	})

	t.Run("server plain, C2 does not correspond", func(t *testing.T) {
		input := append([]byte{3}, bytes.Repeat([]byte{0xAA}, 1536+1536)...)
		rw := newSplitReadWriter(input)
		keyIn, keyOut, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.ErrorIs(t, err, handshake.ErrPeerInvalidData)
		require.Nil(t, keyIn)
		require.Nil(t, keyOut)
	})
}

func TestHandshakerConcurrent(t *testing.T) {
	for _, ca := range []string{"plain", "encrypted"} {
		t.Run(ca, func(t *testing.T) {
			rw := &testReadWriter{ch: make(chan []byte)}

			serverHS := handshake.NewServerHandshaker(rw, true)
			clientHS := handshake.NewClientHandshaker(rw, ca == "encrypted", true)

			var serverInKey, serverOutKey []byte
			var serverErr error
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				serverInKey, serverOutKey, serverErr = serverHS.Do()
			}()

			const n = 8
			clientInKeys := make([][]byte, n)
			clientOutKeys := make([][]byte, n)
			clientErrs := make([]error, n)

			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					clientInKeys[i], clientOutKeys[i], clientErrs[i] = clientHS.Do()
				}(i)
			}
			wg.Wait()
			<-serverDone

			require.NoError(t, serverErr)

			// all concurrent calls must return the result of the single
			// handshake that has been performed.
			for i := 0; i < n; i++ {
				require.NoError(t, clientErrs[i])
				require.Equal(t, clientInKeys[0], clientInKeys[i])
				require.Equal(t, clientOutKeys[0], clientOutKeys[i])
			}

			if ca == "encrypted" {
				require.NotNil(t, serverInKey)
				require.Equal(t, serverInKey, clientOutKeys[0])
				require.Equal(t, serverOutKey, clientInKeys[0])
			}

			// repeated calls return the memoized result without
			// touching the connection again.
			keyIn, keyOut, err := clientHS.Do()
			require.NoError(t, err)
			require.Equal(t, clientInKeys[0], keyIn)
			require.Equal(t, clientOutKeys[0], keyOut)
		})
	}
}

func TestHandshakerRepeatAfterFailure(t *testing.T) {
	rw := newSplitReadWriter([]byte{9})
	hs := handshake.NewServerHandshakerWithRand(rw, true, testCryptoRandRead)

	keyIn, keyOut, err1 := hs.Do()
	require.ErrorIs(t, err1, handshake.ErrPeerInvalidData)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
	require.Equal(t, 0, rw.writer.Len())

	// a repeated call must return the same error and must not
	// write anything to the connection.
	_, _, err2 := hs.Do()
	require.Equal(t, err1, err2)
	require.Equal(t, 0, rw.writer.Len())
}
