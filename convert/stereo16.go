package convert

import (
	"errors"
	"io"
	"sync"

	"github.com/qiniu/audio"
)

// -------------------------------------------------------------------------------------

// Stereo16 class.
type Stereo16 struct {
	source io.ReadSeeker
	mu     sync.Mutex
	pos    int64 // current position in source bytes
	mono   bool
	eight  bool
}

// NewStereo16 func.
func NewStereo16(source io.ReadSeeker, isMono, eightBitsPerSample bool) *Stereo16 {
	return &Stereo16{
		source: source,
		mono:   isMono,
		eight:  eightBitsPerSample,
	}
}

// frameSize returns the number of source bytes per sample frame.
func (s *Stereo16) frameSize() int {
	frame := 4
	if s.mono {
		frame >>= 1
	}
	if s.eight {
		frame >>= 1
	}
	return frame
}

func (s *Stereo16) Read(b []uint8) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := len(b)
	if s.mono {
		l >>= 1
	}
	if s.eight {
		l >>= 1
	}
	// Align to whole source frames, so the stream never drifts off frame
	// boundaries and no partial frame is ever expanded.
	l = l / s.frameSize() * s.frameSize()
	if l == 0 {
		return 0, nil
	}
	buf := b[len(b)-l:]
	n, err := io.ReadFull(s.source, buf)
	s.pos += int64(n)
	if err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		} else if err != io.EOF {
			return 0, err
		}
	}
	// Drop a partial trailing frame, if any.
	n = n / s.frameSize() * s.frameSize()
	switch {
	case s.mono && s.eight:
		for i := 0; i < n; i++ {
			v := int16(int(buf[i])*0x101 - (1 << 15))
			b[4*i] = uint8(v)
			b[4*i+1] = uint8(v >> 8)
			b[4*i+2] = uint8(v)
			b[4*i+3] = uint8(v >> 8)
		}
	case s.mono && !s.eight:
		for i := 0; i < (n >> 1); i++ {
			b[4*i] = buf[2*i]
			b[4*i+1] = buf[2*i+1]
			b[4*i+2] = buf[2*i]
			b[4*i+3] = buf[2*i+1]
		}
	case !s.mono && s.eight:
		for i := 0; i < (n >> 1); i++ {
			v0 := int16(int(buf[2*i])*0x101 - (1 << 15))
			v1 := int16(int(buf[2*i+1])*0x101 - (1 << 15))
			b[4*i] = uint8(v0)
			b[4*i+1] = uint8(v0 >> 8)
			b[4*i+2] = uint8(v1)
			b[4*i+3] = uint8(v1 >> 8)
		}
	}
	if s.mono {
		n <<= 1
	}
	if s.eight {
		n <<= 1
	}
	return n, err
}

// Seek func.
func (s *Stereo16) Seek(offset int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mono {
		offset >>= 1
	}
	if s.eight {
		offset >>= 1
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		// Computed from our own logical position: the source may be a
		// buffered stream whose physical position has read ahead.
		abs = s.pos + offset
	case io.SeekEnd:
		pos, err := s.source.Seek(offset, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		s.pos = pos
		return s.logicalPos(), nil
	default:
		return 0, errors.New("convert: invalid whence")
	}
	pos, err := s.source.Seek(abs, io.SeekStart)
	if err != nil {
		return 0, err
	}
	s.pos = pos
	return s.logicalPos(), nil
}

func (s *Stereo16) logicalPos() int64 {
	pos := s.pos
	if s.eight {
		pos <<= 1
	}
	if s.mono {
		pos <<= 1
	}
	return pos
}

// -------------------------------------------------------------------------------------

type stereo16Decoded struct {
	*Stereo16
	d audio.Decoded
}

// ToStereo16 convert an audio into stereo16.
func ToStereo16(d audio.Decoded) audio.Decoded {
	mono := (d.Channels() == 1)
	eight := (d.BytesPerSample() == 1)
	if mono || eight {
		s := NewStereo16(d, mono, eight)
		return &stereo16Decoded{s, d}
	}
	return d
}

// SampleRate returns the sample rate like 44100.
func (p *stereo16Decoded) SampleRate() int {
	return p.d.SampleRate()
}

// Channels returns the number of channels. One channel is mono playback.
// Two channels are stereo playback. No other values are supported.
func (p *stereo16Decoded) Channels() int {
	return 2
}

// BytesPerSample returns the number of bytes per sample per channel.
// The usual value is 2. Only values 1 and 2 are supported.
func (p *stereo16Decoded) BytesPerSample() int {
	return 2
}

// Length returns the total size in bytes. It returns -1 when the total size is not
// available. e.g. when the given source is not io.Seeker.
func (p *stereo16Decoded) Length() int64 {
	d := p.d
	return (d.Length() << 2) / int64(d.Channels()*d.BytesPerSample())
}

// -------------------------------------------------------------------------------------
