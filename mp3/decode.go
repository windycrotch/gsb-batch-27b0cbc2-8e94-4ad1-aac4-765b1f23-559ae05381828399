package mp3

import (
	"io"
	"sync"

	"github.com/qiniu/audio"
	"github.com/qiniu/x/bufiox"

	mp3 "github.com/hajimehoshi/go-mp3"
)

// -------------------------------------------------------------------------------------

type decoded struct {
	mp3.Decoder
	mu sync.Mutex
}

// Read reads from the decoded stream. It is serialized with Seek so
// concurrent goroutines cannot observe each other's positions.
func (p *decoded) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Decoder.Read(b)
}

// Seek sets the position of the decoded stream.
func (p *decoded) Seek(offset int64, whence int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Decoder.Seek(offset, whence)
}

// Channels returns the number of channels. One channel is mono playback.
// Two channels are stereo playback. No other values are supported.
func (p *decoded) Channels() int {
	return 2
}

// BytesPerSample returns the number of bytes per sample per channel.
// The usual value is 2. Only values 1 and 2 are supported.
func (p *decoded) BytesPerSample() int {
	return 2
}

// Decode decodes a mp3 audio.
func Decode(r io.ReadSeeker) (audio.Decoded, error) {
	b := bufiox.NewReader(r)
	dec, err := mp3.NewDecoder(b)
	if err != nil {
		return nil, err
	}
	return &decoded{Decoder: *dec}, err
}

// DecodeConfig is not implemented.
func DecodeConfig(r io.ReadSeeker) (cfg audio.Config, err error) {
	err = audio.ErrFormat
	return
}

func init() {
	audio.RegisterFormat("mp3", "ID3", Decode, DecodeConfig)
	audio.RegisterFormat("mp3", "\xff\xfb", Decode, DecodeConfig)
}

// -------------------------------------------------------------------------------------
