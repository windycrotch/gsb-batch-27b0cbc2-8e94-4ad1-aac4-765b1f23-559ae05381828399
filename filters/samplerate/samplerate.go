// Package samplerate contains a sample rate converter using linear
// interpolation.
package samplerate

import (
	"github.com/1lann/dissonance/audio"
)

// Filter represents the sample rate audio.Filter
type Filter struct {
	sampleRate int
}

type streamFilter struct {
	stream     audio.Stream
	sampleRate int
	// buffer holds input samples that have not been fully consumed yet.
	// At least one sample is retained between Reads so that interpolation
	// stays continuous across Read boundaries.
	buffer []int32
	// pos is the position, in buffer indices, at which the next output
	// sample must be interpolated. It carries the fractional interpolation
	// position across Reads instead of being reset each time.
	pos   float64
	ratio float64
	// done records that the upstream stream has ended. Any buffered tail
	// samples are still emitted first; only once no further output can be
	// produced is the terminal error surfaced.
	done   bool
	endErr error
}

// NewFilter returns a new sample rate filter to convert into the given
// sample rate.
func NewFilter(sampleRate int) audio.Filter {
	return &Filter{sampleRate: sampleRate}
}

func interpolate(buffer []int32, position float64) int32 {
	i := int(position)
	between := position - float64(i)
	return int32(float64(buffer[i]) + float64(buffer[i+1]-buffer[i])*between)
}

// Filter implements the Filter method for filters.
func (f *Filter) Filter(stream audio.Stream) audio.Stream {
	return &streamFilter{
		stream:     stream,
		sampleRate: f.sampleRate,
		buffer:     []int32{},
		pos:        0,
		ratio:      float64(stream.SampleRate()) / float64(f.sampleRate),
	}
}

func (f *streamFilter) SampleRate() int {
	return f.sampleRate
}

// Read resamples upstream samples into dst. The interpolation position is
// preserved between calls, so splitting a stream across many Reads yields
// exactly the same output as converting it in one go, and the number of
// produced samples grows monotonically with the amount of input consumed.
func (f *streamFilter) Read(dst interface{}) (int, error) {
	dstLen := audio.SliceLength(dst)
	if dstLen == 0 {
		return 0, nil
	}

	result := make([]int32, 0, dstLen)
	emptyReads := 0
	for len(result) < dstLen {
		// Pull enough input to interpolate every output still missing.
		targetLen := int(f.pos+float64(dstLen-len(result))*f.ratio) + 2
		if needed := targetLen - len(f.buffer); needed > 0 && !f.done {
			buf := make([]int32, needed)
			n, err := f.stream.Read(buf)
			// Honor n: the upstream may return fewer samples together with
			// an error (including io.EOF), and those samples are valid.
			if n > 0 {
				f.buffer = append(f.buffer, buf[:n]...)
			}
			if err != nil {
				f.done = true
				f.endErr = err
			}
			if n == 0 && err == nil {
				// Upstream temporarily has nothing. If we already hold
				// outputs, hand them back; otherwise retry a bounded number
				// of times, then return (0, nil) so a misbehaving source
				// cannot pin this goroutine forever.
				if len(result) > 0 || emptyReads >= 100 {
					break
				}
				emptyReads++
				continue
			}
			emptyReads = 0
		}

		before := len(result)
		for len(result) < dstLen && int(f.pos)+1 < len(f.buffer) {
			result = append(result, interpolate(f.buffer, f.pos))
			f.pos += f.ratio
		}

		if len(result) > before {
			// Only evict once some outputs were actually produced:
			// otherwise the retained neighbour samples would be discarded
			// and interpolation continuity would break across Reads.
			//
			// Evict the fully consumed prefix while always keeping at least
			// one sample, and shift the interpolation position by exactly
			// the number of evicted samples so continuity is preserved.
			keepFrom := int(f.pos)
			if keepFrom > len(f.buffer)-1 {
				keepFrom = len(f.buffer) - 1
			}
			if keepFrom > 0 {
				f.buffer = f.buffer[keepFrom:]
				f.pos -= float64(keepFrom)
			}
		}

		if f.done {
			// Tail samples already buffered were just emitted above; only
			// exit once nothing more can be produced.
			break
		}
	}

	if len(result) > 0 {
		if err := audio.ReadFromInt32(dst, result, len(result)); err != nil {
			return 0, err
		}
		return len(result), nil
	}

	// Nothing more can be produced: surface the terminal error (io.EOF for
	// a gracefully ended upstream, or the error it failed with).
	return 0, f.endErr
}
