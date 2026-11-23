package rtmp

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestHandshakeSimpleFallback(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- ServerHandshake(serverConn)
	}()

	c0c1 := makeSimpleC0C1(t)
	if _, err := clientConn.Write(c0c1); err != nil {
		t.Fatalf("write C0C1: %v", err)
	}

	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	if _, err := io.ReadFull(clientConn, s0s1s2); err != nil {
		t.Fatalf("read S0S1S2: %v", err)
	}
	if s0s1s2[0] != 3 {
		t.Fatalf("S0 version = %d, want 3", s0s1s2[0])
	}
	s1, s2 := s0s1s2[1:1+handshakeSize], s0s1s2[1+handshakeSize:]
	if bytes.Equal(s2, c0c1[1:]) {
		t.Fatal("simple-handshake S2 mirrored C1 verbatim")
	}
	if !bytes.Equal(s2[:4], c0c1[1:5]) {
		t.Error("simple-handshake S2 did not echo the C1 time field")
	}
	// S1 must stay version-less so a client keeps using the simple handshake.
	if !bytes.Equal(s1[4:8], []byte{0, 0, 0, 0}) {
		t.Errorf("simple-handshake S1 version = %x, want zeros", s1[4:8])
	}

	if _, err := clientConn.Write(s1); err != nil {
		t.Fatalf("write C2: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestHandshakeComplexAccepted(t *testing.T) {
	tests := []struct {
		name   string
		schema int
	}{
		{name: "schema1", schema: 1},
		{name: "schema0", schema: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			serverErr := make(chan error, 1)
			go func() {
				serverErr <- ServerHandshake(serverConn)
			}()

			c0, c1, c1DigestOffset := makeComplexC1(t, tt.schema)
			if _, err := clientConn.Write(append(c0, c1...)); err != nil {
				t.Fatalf("write C0C1: %v", err)
			}

			s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
			if _, err := io.ReadFull(clientConn, s0s1s2); err != nil {
				t.Fatalf("read S0S1S2: %v", err)
			}
			s1, s2 := s0s1s2[1:1+handshakeSize], s0s1s2[1+handshakeSize:]

			if _, ok := findDigest(s1, genuineFMSKey); !ok {
				t.Fatal("S1 digest did not verify with the FMS key")
			}
			if !verifyS2Digest(c1[c1DigestOffset:c1DigestOffset+32], s2) {
				t.Fatal("S2 digest did not verify for the C1 digest")
			}

			if _, err := clientConn.Write(s1); err != nil {
				t.Fatalf("write C2: %v", err)
			}
			if err := <-serverErr; err != nil {
				t.Fatalf("server handshake: %v", err)
			}
		})
	}
}

func TestHandshakeComplexForgedDigestRejected(t *testing.T) {
	tests := []struct {
		name   string
		schema int
	}{
		{name: "schema1", schema: 1},
		{name: "schema0", schema: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			serverErr := make(chan error, 1)
			go func() {
				serverErr <- ServerHandshake(serverConn)
			}()

			c0, c1, _ := makeComplexC1(t, tt.schema)
			// Keep the non-zero client version that announces the digest
			// handshake, but corrupt a byte the digest protects.
			c1[0] ^= 0xFF
			if _, err := clientConn.Write(append(c0, c1...)); err != nil {
				t.Fatalf("write C0C1: %v", err)
			}

			if err := <-serverErr; !errors.Is(err, ErrInvalidC1Digest) {
				t.Fatalf("handshake error = %v, want ErrInvalidC1Digest", err)
			}
			// The server must not answer a rejected handshake.
			if err := clientConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}
			var buf [1]byte
			if n, err := clientConn.Read(buf[:]); err == nil || n > 0 {
				t.Fatalf("server answered a rejected complex handshake (n=%d, err=%v)", n, err)
			}
		})
	}
}

func TestClassifyC1SimpleHasNoDigest(t *testing.T) {
	c0c1 := makeSimpleC0C1(t)
	mode, _, err := classifyC1(c0c1[1:])
	if err != nil {
		t.Fatalf("classifyC1: %v", err)
	}
	if mode != handshakeSimple {
		t.Fatalf("mode = %v, want simple", mode)
	}
}

// makeSimpleC0C1 builds C0+C1 for the simple handshake: time and version are
// zero and the remainder is random.
func makeSimpleC0C1(t *testing.T) []byte {
	t.Helper()
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = 3
	if _, err := rand.Read(c0c1[9:]); err != nil {
		t.Fatalf("generate C1: %v", err)
	}
	return c0c1
}

// makeComplexC1 builds C0+C1 for the digest handshake. Schema 1 derives the
// digest offset from bytes 8-11; schema 0 derives it from bytes 772-775.
func makeComplexC1(t *testing.T, schema int) (c0, c1 []byte, digestOffset int) {
	t.Helper()
	c0 = []byte{3}
	c1 = make([]byte, handshakeSize)
	// Client version (non-zero) announces the digest handshake.
	copy(c1[4:8], []byte{0x80, 0x00, 0x07, 0x02})
	if _, err := rand.Read(c1[8:]); err != nil {
		t.Fatalf("generate C1: %v", err)
	}

	switch schema {
	case 1:
		digestOffset = calcDigestOffset(c1[8:12], 12)
	case 0:
		digestOffset = calcDigestOffset(c1[772:776], 776)
	default:
		t.Fatalf("unknown schema %d", schema)
	}

	msg := make([]byte, 0, handshakeSize-32)
	msg = append(msg, c1[:digestOffset]...)
	msg = append(msg, c1[digestOffset+32:]...)
	h := hmac.New(sha256.New, genuineFPKey)
	h.Write(msg)
	copy(c1[digestOffset:], h.Sum(nil))
	return c0, c1, digestOffset
}

// verifyS2Digest checks the S2 packet the way a complex-handshake client does.
func verifyS2Digest(c1Digest, s2 []byte) bool {
	keyH := hmac.New(sha256.New, genuineFMSKeyFull)
	keyH.Write(c1Digest)
	h := hmac.New(sha256.New, keyH.Sum(nil))
	h.Write(s2[:handshakeSize-32])
	return hmac.Equal(h.Sum(nil), s2[handshakeSize-32:])
}
