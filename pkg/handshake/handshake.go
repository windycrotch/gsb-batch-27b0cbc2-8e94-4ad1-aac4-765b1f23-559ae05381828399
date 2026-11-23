// Package handshake contains the RTMP handshake mechanism.
package handshake

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
)

const (
	encryptedVersion = 3<<24 | 5<<16 | 1<<8 | 1
)

// prefetchRandRead reads upfront all the random data needed by a handshake,
// and returns a function that serves the prefetched data.
// This allows to detect a failure of the random source before
// writing any byte to the connection.
func prefetchRandRead(randRead func([]byte) (int, error), sizes ...int) (func([]byte) (int, error), error) {
	var buf []byte
	for _, size := range sizes {
		tmp := make([]byte, size)
		_, err := randRead(tmp)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRandomSource, err)
		}
		buf = append(buf, tmp...)
	}
	r := bytes.NewReader(buf)
	return r.Read, nil
}

func doClientEncrypted(rw io.ReadWriter, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	// prefetch all random data before writing anything to the connection.
	randRead, err := prefetchRandRead(randRead, dhKeyLength, c1s1Size-8+1, c2s2Size-8)
	if err != nil {
		return nil, nil, err
	}

	localPrivateKey, localPublicKey, err := dhGenerateKeyPair(randRead)
	if err != nil {
		return nil, nil, err
	}

	var c1 C1S1

	c1Digest, err := c1.fill(false, localPublicKey, randRead)
	if err != nil {
		return nil, nil, err
	}

	var c2 C2S2

	err = c2.fillPlain(randRead)
	if err != nil {
		return nil, nil, err
	}

	var c0 C0S0

	c0.Version = 6

	err = c0.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	err = c1.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var s0 C0S0

	err = s0.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	if s0.Version != 6 {
		return nil, nil, invalidDataError(fmt.Errorf("server replied with unexpected version %d", s0.Version))
	}

	var s1 C1S1

	err = s1.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	s1Digest, remotePublicKey, err := s1.validate(true)
	if err != nil {
		return nil, nil, invalidDataError(err)
	}

	var s2 C2S2

	err = s2.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	err = s2.validate(true, c1Digest)
	if err != nil {
		return nil, nil, invalidDataError(err)
	}

	// all checks passed: complete C2 with the digest of S1 and send it.
	c2Digest := c2.computeDigest(false, s1Digest)
	copy(c2.Data[c2s2DigestPos:], c2Digest)

	err = c2.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	// return keys only after every digest and public key has been validated.
	sharedSecret := dhComputeSharedSecret(localPrivateKey, remotePublicKey)
	keyIn := hmacSha256(sharedSecret, localPublicKey)[:16]
	keyOut := hmacSha256(sharedSecret, remotePublicKey)[:16]
	return keyIn, keyOut, nil
}

func doClientPlain(rw io.ReadWriter, strict bool, randRead func([]byte) (int, error)) error {
	// prefetch all random data before writing anything to the connection.
	randRead, err := prefetchRandRead(randRead, c1s1Size-8)
	if err != nil {
		return err
	}

	var c1 C1S1

	err = c1.fillPlain(randRead)
	if err != nil {
		return err
	}

	var c0 C0S0

	c0.Version = 3

	err = c0.Write(rw)
	if err != nil {
		return err
	}

	err = c1.Write(rw)
	if err != nil {
		return err
	}

	var s0 C0S0

	err = s0.Read(rw)
	if err != nil {
		return classifyReadError(err)
	}

	if s0.Version != 3 {
		return invalidDataError(fmt.Errorf("server replied with unexpected version %d", s0.Version))
	}

	var s1 C1S1

	err = s1.Read(rw)
	if err != nil {
		return classifyReadError(err)
	}

	var s2 C2S2

	err = s2.Read(rw)
	if err != nil {
		return classifyReadError(err)
	}

	if strict && !bytes.Equal(s2.Data, c1.Data) {
		return invalidDataError(fmt.Errorf("data in S2 does not correspond"))
	}

	var c2 C2S2

	c2.Data = s1.Data

	return c2.Write(rw)
}

// DoClient performs a client-side handshake.
func DoClient(rw io.ReadWriter, encrypted bool, strict bool) ([]byte, []byte, error) {
	return DoClientWithRand(rw, encrypted, strict, rand.Read)
}

// DoClientWithRand performs a client-side handshake, using the given function
// to generate random data.
func DoClientWithRand(
	rw io.ReadWriter,
	encrypted bool,
	strict bool,
	randRead func([]byte) (int, error),
) ([]byte, []byte, error) {
	if encrypted {
		return doClientEncrypted(rw, randRead)
	}
	return nil, nil, doClientPlain(rw, strict, randRead)
}

func doServerEncrypted(rw io.ReadWriter, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	// prefetch all random data before writing anything to the connection.
	randRead, err := prefetchRandRead(randRead, dhKeyLength, c1s1Size-8+1, c2s2Size-8)
	if err != nil {
		return nil, nil, err
	}

	localPrivateKey, localPublicKey, err := dhGenerateKeyPair(randRead)
	if err != nil {
		return nil, nil, err
	}

	var s1 C1S1

	s1.Version = encryptedVersion

	s1Digest, err := s1.fill(true, localPublicKey, randRead)
	if err != nil {
		return nil, nil, err
	}

	var s2 C2S2

	s2.Time2 = encryptedVersion

	err = s2.fillPlain(randRead)
	if err != nil {
		return nil, nil, err
	}

	var c1 C1S1

	err = c1.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	c1Digest, remotePublicKey, err := c1.validate(false)
	if err != nil {
		return nil, nil, invalidDataError(err)
	}

	// C1 is valid: complete S2 with the digest of C1.
	s2Digest := s2.computeDigest(true, c1Digest)
	copy(s2.Data[c2s2DigestPos:], s2Digest)

	var s0 C0S0

	s0.Version = 6

	err = s0.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	err = s1.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	err = s2.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var c2 C2S2

	err = c2.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	err = c2.validate(false, s1Digest)
	if err != nil {
		return nil, nil, invalidDataError(err)
	}

	// return keys only after every digest and public key has been validated.
	sharedSecret := dhComputeSharedSecret(localPrivateKey, remotePublicKey)
	keyIn := hmacSha256(sharedSecret, localPublicKey)[:16]
	keyOut := hmacSha256(sharedSecret, remotePublicKey)[:16]
	return keyIn, keyOut, nil
}

func doServerPlain(rw io.ReadWriter, strict bool, randRead func([]byte) (int, error)) error {
	// prefetch all random data before writing anything to the connection.
	randRead, err := prefetchRandRead(randRead, c1s1Size-8)
	if err != nil {
		return err
	}

	var s1 C1S1

	err = s1.fillPlain(randRead)
	if err != nil {
		return err
	}

	var c1 C1S1

	err = c1.Read(rw)
	if err != nil {
		return classifyReadError(err)
	}

	var s0 C0S0

	s0.Version = 3

	err = s0.Write(rw)
	if err != nil {
		return err
	}

	err = s1.Write(rw)
	if err != nil {
		return err
	}

	var s2 C2S2

	s2.Data = c1.Data

	err = s2.Write(rw)
	if err != nil {
		return err
	}

	var c2 C2S2

	err = c2.Read(rw)
	if err != nil {
		return classifyReadError(err)
	}

	if strict && !bytes.Equal(c2.Data, s1.Data) {
		return invalidDataError(fmt.Errorf("data in C2 does not correspond"))
	}

	return nil
}

// DoServer performs a server-side handshake.
func DoServer(rw io.ReadWriter, strict bool) ([]byte, []byte, error) {
	return DoServerWithRand(rw, strict, rand.Read)
}

// DoServerWithRand performs a server-side handshake, using the given function
// to generate random data.
func DoServerWithRand(rw io.ReadWriter, strict bool, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	var c0 C0S0

	err := c0.Read(rw)
	if err != nil {
		return nil, nil, classifyReadError(err)
	}

	if c0.Version == 6 {
		return doServerEncrypted(rw, randRead)
	}

	return nil, nil, doServerPlain(rw, strict, randRead)
}
