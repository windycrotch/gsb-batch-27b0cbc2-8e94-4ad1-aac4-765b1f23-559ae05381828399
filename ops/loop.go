// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"io"

	"github.com/zikichombo/sound"
)

type loop struct {
	sound.SourceSeeker
	// n is the number of complete copies of the source which remain
	// to be delivered.  A negative value means loop forever.
	n int
}

// Receive implements sound.Source.
//
// The output is the source repeated n times: Receive fills dst with the
// prefix of that repeated stream, seeking back to the start of the source
// whenever its end is reached.  Wrap-around is handled frame by frame and
// is independent of the channel count, so multi-channel data never crosses
// channels, even when a single Receive wraps multiple times.
//
// No more than len(dst)/Channels() frames are returned.  When the requested
// number of repeats is exhausted, the final short Receive returns the
// remaining frames with a nil error and the following call returns
// (0, io.EOF).
func (l *loop) Receive(dst []float64) (int, error) {
	nC := l.Channels()
	if len(dst)%nC != 0 {
		return 0, sound.ErrChannelAlignment
	}
	if l.n == 0 {
		return 0, io.EOF
	}
	want := len(dst) / nC
	// il accumulates the repeated stream in interleaved order (frame 0's
	// channels, then frame 1's, ...).  In interleaved order appending the
	// next read at a running sample offset is always correct, so wrap-around
	// cannot cross channels regardless of how many times the source ends
	// within this call.
	il := make([]float64, 0, want*nC)
	// Each underlying read is de-interleaved over the frames it returns.
	raw := make([]float64, want*nC)
	for len(il)/nC < want {
		remain := want - len(il)/nC
		n, err := l.SourceSeeker.Receive(raw[:remain*nC])
		if err != nil && err != io.EOF {
			return 0, err
		}
		if n > 0 {
			// Convert this read's de-interleaved n frames to interleaved.
			for f := 0; f < n; f++ {
				for c := 0; c < nC; c++ {
					il = append(il, raw[c*n+f])
				}
			}
		}
		if err == io.EOF {
			// The source reached its end, completing one delivered copy.
			// A negative repeat count means loop forever.
			if l.n > 0 {
				l.n--
			}
			if l.n == 0 {
				// No further wrap-around is allowed; the unread tail of
				// this final copy has already been appended above.
				break
			}
			if serr := l.Seek(0); serr != nil {
				return 0, serr
			}
		}
	}
	filled := len(il) / nC
	if filled == 0 {
		return 0, io.EOF
	}
	// Lay the interleaved result into dst de-interleaved over the actual
	// number of frames produced (which may be smaller than want on the
	// final short Receive).
	for f := 0; f < filled; f++ {
		for c := 0; c < nC; c++ {
			dst[c*filled+f] = il[f*nC+c]
		}
	}
	return filled, nil
}

// Loop creates a src which is src looped n times.
// Loop will loop infinitely if n < 0 and will
// terminate with EOF immediately if n == 0.
func Loop(src sound.SourceSeeker, n int) sound.Source {
	return &loop{SourceSeeker: src, n: n}
}
