package audio_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/qiniu/audio"
	_ "github.com/qiniu/audio/wav"
)

// TestSniffShortFiles checks that probing short files never panics and
// reports the recognizable ErrFormat when no registered magic matches.
func TestSniffShortFiles(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"one-byte", []byte{'R'}},
		{"riff-prefix", []byte("RIFF")},                         // prefix of the wav magic
		{"riff-half", []byte("RIFF\x00\x00")},                   // shorter than the 12-byte magic
		{"riff-no-wave", []byte("RIFF\x00\x00\x00\x00XXXX")},    // full length, WAVE mismatch
		{"garbage", []byte("this is not an audio file at all")}, // no registered magic
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := audio.Decode(bytes.NewReader(c.data))
			if !errors.Is(err, audio.ErrFormat) {
				t.Fatalf("Decode: err = %v, want ErrFormat", err)
			}
			_, _, err = audio.DecodeConfig(bytes.NewReader(c.data))
			if !errors.Is(err, audio.ErrFormat) {
				t.Fatalf("DecodeConfig: err = %v, want ErrFormat", err)
			}
		})
	}
}

// TestSniffLongMagicCandidate registers a format whose magic is longer than
// the probed file and checks the file is still matched against other
// registered formats instead of being misjudged as unknown.
func TestSniffLongMagicCandidate(t *testing.T) {
	const magic = "A-VERY-LONG-MAGIC-HEADER"
	audio.RegisterFormat("longmagic", magic+"????????", func(io.ReadSeeker) (audio.Decoded, error) {
		return nil, audio.ErrFormat
	}, nil)

	// A valid wav file is much shorter than the magic above when only its
	// first bytes are considered; probing must still find "wav".
	data := buildWAVForSniff()
	_, format, err := audio.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if format != "wav" {
		t.Fatalf("format = %q, want wav", format)
	}

	// A file that is a strict prefix of the long magic must not match it.
	_, _, err = audio.Decode(bytes.NewReader([]byte("A-VERY")))
	if !errors.Is(err, audio.ErrFormat) {
		t.Fatalf("Decode prefix of long magic: err = %v, want ErrFormat", err)
	}
}

func buildWAVForSniff() []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	b.Write([]byte{36, 0, 0, 0})
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	b.Write([]byte{16, 0, 0, 0, 1, 0, 2, 0, 0x44, 0xac, 0, 0, 0x10, 0xb1, 2, 0, 4, 0, 16, 0})
	b.WriteString("data")
	b.Write([]byte{0, 0, 0, 0})
	return b.Bytes()
}
