package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

func TestChunkSetChunkSizeRejectsZero(t *testing.T) {
	reader := NewChunkReader(bytes.NewReader(nil), DefaultChunkSize)
	if err := reader.SetChunkSize(0); !errors.Is(err, ErrInvalidChunkSize) {
		t.Fatalf("SetChunkSize(0) error = %v, want ErrInvalidChunkSize", err)
	}
	if got := reader.ChunkSize(); got != DefaultChunkSize {
		t.Fatalf("chunk size = %d, want unchanged %d", got, DefaultChunkSize)
	}

	writer := NewChunkWriter(&bytes.Buffer{}, DefaultChunkSize)
	if err := writer.SetChunkSize(0); !errors.Is(err, ErrInvalidChunkSize) {
		t.Fatalf("writer SetChunkSize(0) error = %v, want ErrInvalidChunkSize", err)
	}
}

func TestChunkSetChunkSizeRejectsAboveLimit(t *testing.T) {
	reader := NewChunkReader(bytes.NewReader(nil), DefaultChunkSize)
	for _, size := range []int{MaxChunkSize + 1, 1 << 30} {
		if err := reader.SetChunkSize(size); !errors.Is(err, ErrInvalidChunkSize) {
			t.Fatalf("SetChunkSize(%d) error = %v, want ErrInvalidChunkSize", size, err)
		}
	}
	if got := reader.ChunkSize(); got != DefaultChunkSize {
		t.Fatalf("chunk size = %d, want unchanged %d", got, DefaultChunkSize)
	}

	writer := NewChunkWriter(&bytes.Buffer{}, DefaultChunkSize)
	if err := writer.SetChunkSize(MaxChunkSize + 1); !errors.Is(err, ErrInvalidChunkSize) {
		t.Fatalf("writer SetChunkSize above limit error = %v, want ErrInvalidChunkSize", err)
	}
}

func TestChunkSetChunkSizeAcceptsUpperBound(t *testing.T) {
	reader := NewChunkReader(bytes.NewReader(nil), DefaultChunkSize)
	if err := reader.SetChunkSize(MaxChunkSize); err != nil {
		t.Fatalf("SetChunkSize(MaxChunkSize) error = %v", err)
	}
	if got := reader.ChunkSize(); got != MaxChunkSize {
		t.Fatalf("chunk size = %d, want %d", got, MaxChunkSize)
	}
}

func TestChunkSetChunkSizeRejectedMidMessage(t *testing.T) {
	// A 12-byte message on CSID 4 with chunk size 4 spans three chunks. Only
	// the first is supplied, so the message stays incomplete.
	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 0,
		0, 0, 12,
		MsgVideo,
		1, 0, 0, 0,
		1, 2, 3, 4,
	}
	reader := NewChunkReader(bytes.NewReader(wire), 4)
	if _, err := reader.ReadMessage(); err == nil {
		t.Fatal("expected a read error for the truncated message")
	}
	if err := reader.SetChunkSize(8); !errors.Is(err, ErrChunkSizeChangedMidMessage) {
		t.Fatalf("SetChunkSize mid-message error = %v, want ErrChunkSizeChangedMidMessage", err)
	}
	if got := reader.ChunkSize(); got != 4 {
		t.Fatalf("chunk size = %d, want unchanged 4", got)
	}
}

func TestChunkReadCompleteMessageAllowsSizeChange(t *testing.T) {
	wire := []byte{
		byte(chunkFmt0<<6 | 4),
		0, 0, 0,
		0, 0, 2,
		MsgAudio,
		1, 0, 0, 0,
		0xAA, 0xBB,
	}
	reader := NewChunkReader(bytes.NewReader(wire), 4)
	if _, err := reader.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if err := reader.SetChunkSize(8); err != nil {
		t.Fatalf("SetChunkSize after complete message: %v", err)
	}
}

func TestHandlerRejectsZeroChunkSize(t *testing.T) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, 0)
	if err := handleControlForTest(t, &Message{TypeID: MsgSetChunkSize, Length: 4, Payload: payload}); !errors.Is(err, ErrInvalidChunkSize) {
		t.Fatalf("SetChunkSize(0) handler error = %v, want ErrInvalidChunkSize", err)
	}
}

func TestHandlerRejectsOversizeChunkSize(t *testing.T) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, MaxChunkSize+1)
	if err := handleControlForTest(t, &Message{TypeID: MsgSetChunkSize, Length: 4, Payload: payload}); !errors.Is(err, ErrInvalidChunkSize) {
		t.Fatalf("oversize chunk handler error = %v, want ErrInvalidChunkSize", err)
	}
}

func TestHandlerRejectsChunkSizeChangeMidMessage(t *testing.T) {
	// Chunk size 4: CSID 4 starts a 12-byte message and only its first chunk is
	// written. A complete SetChunkSize message on CSID 2 then follows while that
	// message is still being assembled.
	setChunkPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(setChunkPayload, 65536)

	var wire bytes.Buffer
	writer := NewChunkWriter(&wire, 4)
	if err := writer.WriteMessage(4, &Message{TypeID: MsgVideo, Length: 12, Payload: []byte{1, 2, 3, 4}}); err != nil {
		t.Fatalf("write partial message: %v", err)
	}
	firstChunk := append([]byte(nil), wire.Bytes()[:1+11+4]...)
	wire.Reset()
	if err := writer.WriteMessage(2, &Message{TypeID: MsgSetChunkSize, Length: 4, Payload: setChunkPayload}); err != nil {
		t.Fatalf("write SetChunkSize: %v", err)
	}
	wireBytes := append(firstChunk, wire.Bytes()...)

	reader := NewChunkReader(bytes.NewReader(wireBytes), 4)
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("read SetChunkSize: %v", err)
	}
	if msg.TypeID != MsgSetChunkSize {
		t.Fatalf("message type = %d, want SetChunkSize", msg.TypeID)
	}
	if err := reader.SetChunkSize(65536); !errors.Is(err, ErrChunkSizeChangedMidMessage) {
		t.Fatalf("mid-message SetChunkSize error = %v, want ErrChunkSizeChangedMidMessage", err)
	}
	if got := reader.ChunkSize(); got != 4 {
		t.Fatalf("chunk size = %d, want unchanged 4", got)
	}
}

// handleControlForTest routes one control message through the production
// handler dispatch with an in-memory connection.
func handleControlForTest(t *testing.T, msg *Message) error {
	t.Helper()
	hub, bus := newTestHub()
	conn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})
	handler := NewHandler(conn, hub, bus, 4096, nil)
	return handler.handleMessage(msg)
}

func TestChunkConstructorsNormalizeOutOfRangeSize(t *testing.T) {
	reader := NewChunkReader(bytes.NewReader(nil), 0)
	if got := reader.ChunkSize(); got != DefaultChunkSize {
		t.Fatalf("reader chunk size = %d, want %d", got, DefaultChunkSize)
	}
	writer := NewChunkWriter(&bytes.Buffer{}, MaxChunkSize+1)
	if got := writer.ChunkSize(); got != DefaultChunkSize {
		t.Fatalf("writer chunk size = %d, want %d", got, DefaultChunkSize)
	}
}
