package adpcm_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/qiniu/audio"
	_ "github.com/qiniu/audio/wav"
	_ "github.com/qiniu/audio/wav/adpcm"
)

// buildADPCMWAV builds an IMA ADPCM wav file in memory.
func buildADPCMWAV(channels, sampleRate, blockAlign int, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	writeU32(&b, uint32(36+len(data)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	writeU32(&b, 16)
	writeU16(&b, 0x11) // IMA ADPCM
	writeU16(&b, uint16(channels))
	writeU32(&b, uint32(sampleRate))
	writeU32(&b, uint32(sampleRate*blockAlign/505)) // approx byte rate, unused
	writeU16(&b, uint16(blockAlign))
	writeU16(&b, 4) // bits per sample
	b.WriteString("data")
	writeU32(&b, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func writeU16(b *bytes.Buffer, v uint16) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
}

func writeU32(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v >> 16))
	b.WriteByte(byte(v >> 24))
}

func TestDecodeADPCM(t *testing.T) {
	const blockAlign = 256
	const blocks = 2
	data := make([]byte, blockAlign*blocks)
	for i := range data {
		data[i] = byte(i * 31)
	}
	wav := buildADPCMWAV(1, 8000, blockAlign, data)
	d, format, err := audio.Decode(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if format != "wav" {
		t.Fatalf("format = %q, want wav", format)
	}
	samplesPerBlock := blockAlign*2 - 7
	wantLen := int64(blocks * samplesPerBlock * 2)
	if d.Length() != wantLen {
		t.Fatalf("Length = %d, want %d", d.Length(), wantLen)
	}
	out, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if int64(len(out)) != wantLen {
		t.Fatalf("decoded %d bytes, want %d", len(out), wantLen)
	}
}

// TestDecodeADPCMBadHeaders checks that crafted headers produce errors,
// not panics.
func TestDecodeADPCMBadHeaders(t *testing.T) {
	cases := []struct {
		name       string
		channels   int
		blockAlign int
		data       []byte
	}{
		{"tiny-block", 1, 2, make([]byte, 2)},
		{"block-too-small-for-channels", 2, 4, make([]byte, 4)},
		{"huge-block", 1, 1 << 30, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wav := buildADPCMWAV(c.channels, 8000, c.blockAlign, c.data)
			if _, _, err := audio.Decode(bytes.NewReader(wav)); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}

// TestDecodeADPCMBadIndex checks that a crafted predictor index byte in the
// block header cannot drive the step table out of bounds.
func TestDecodeADPCMBadIndex(t *testing.T) {
	const blockAlign = 256
	data := make([]byte, blockAlign)
	data[2] = 0xff // crafted status index, must be clamped
	wav := buildADPCMWAV(1, 8000, blockAlign, data)
	d, _, err := audio.Decode(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := io.ReadAll(d); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
}
