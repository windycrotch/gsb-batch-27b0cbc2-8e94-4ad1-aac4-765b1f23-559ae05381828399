package handshake

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

type internalSplitRW struct {
	reader bytes.Reader
	writer bytes.Buffer
}

func (rw *internalSplitRW) Read(p []byte) (int, error) {
	return rw.reader.Read(p)
}

func (rw *internalSplitRW) Write(p []byte) (int, error) {
	return rw.writer.Write(p)
}

func internalRandRead(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x07
	}
	return len(p), nil
}

func TestDoClientEncryptedInvalidPublicKey(t *testing.T) {
	// craft an S1 with a valid digest but an invalid public key
	// (all zeros -> y < 2).
	var s1 C1S1
	s1.Version = encryptedVersion
	_, err := s1.fill(true, make([]byte, dhKeyLength), internalRandRead)
	require.NoError(t, err)

	var buf bytes.Buffer
	buf.WriteByte(6)
	err = s1.Write(&buf)
	require.NoError(t, err)

	rw := &internalSplitRW{reader: *bytes.NewReader(buf.Bytes())}
	keyIn, keyOut, err := DoClientWithRand(rw, true, false, internalRandRead)
	require.ErrorIs(t, err, ErrPeerInvalidData)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
	// C2 must not be written after the failure.
	require.Equal(t, 1537, rw.writer.Len())
}

func TestDoServerEncryptedInvalidPublicKey(t *testing.T) {
	// craft a C1 with a valid digest but an invalid public key.
	var c1 C1S1
	_, err := c1.fill(false, make([]byte, dhKeyLength), internalRandRead)
	require.NoError(t, err)

	var buf bytes.Buffer
	buf.WriteByte(6)
	err = c1.Write(&buf)
	require.NoError(t, err)

	rw := &internalSplitRW{reader: *bytes.NewReader(buf.Bytes())}
	keyIn, keyOut, err := DoServerWithRand(rw, false, internalRandRead)
	require.ErrorIs(t, err, ErrPeerInvalidData)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
	// nothing must be written after the failure.
	require.Equal(t, 0, rw.writer.Len())
}

func TestDoClientEncryptedInvalidS2Digest(t *testing.T) {
	// craft a valid S0+S1 pair, followed by an S2 with an invalid digest.
	_, pub, err := dhGenerateKeyPair(internalRandRead)
	require.NoError(t, err)

	var s1 C1S1
	s1.Version = encryptedVersion
	_, err = s1.fill(true, pub, internalRandRead)
	require.NoError(t, err)

	var buf bytes.Buffer
	buf.WriteByte(6)
	err = s1.Write(&buf)
	require.NoError(t, err)
	buf.Write(bytes.Repeat([]byte{0xAA}, c2s2Size))

	rw := &internalSplitRW{reader: *bytes.NewReader(buf.Bytes())}
	keyIn, keyOut, err := DoClientWithRand(rw, true, false, internalRandRead)
	require.ErrorIs(t, err, ErrPeerInvalidData)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
	// C2 must not be written after the failure.
	require.Equal(t, 1537, rw.writer.Len())
}

func TestDoServerEncryptedInvalidC2Digest(t *testing.T) {
	// craft a valid C0+C1 pair, followed by a C2 with an invalid digest.
	_, pub, err := dhGenerateKeyPair(internalRandRead)
	require.NoError(t, err)

	var c1 C1S1
	_, err = c1.fill(false, pub, internalRandRead)
	require.NoError(t, err)

	var buf bytes.Buffer
	buf.WriteByte(6)
	err = c1.Write(&buf)
	require.NoError(t, err)
	buf.Write(bytes.Repeat([]byte{0xAA}, c2s2Size))

	rw := &internalSplitRW{reader: *bytes.NewReader(buf.Bytes())}
	keyIn, keyOut, err := DoServerWithRand(rw, false, internalRandRead)
	require.ErrorIs(t, err, ErrPeerInvalidData)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
}
