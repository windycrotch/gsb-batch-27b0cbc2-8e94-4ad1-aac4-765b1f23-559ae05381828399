// The MIT License (MIT)
//
// Copyright (c) 2019 Amangeldy Kadyl
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

// Package pipeline connects frame decoders (lossy VP8, lossless VP8L) to the
// final output image. Decoders produce full-width, non-premultiplied RGBA rows
// (4 bytes per pixel, R,G,B,A order) strictly top to bottom; a Sink decides
// where those rows go (straight into the output image, or through
// crop / scale stages).
//
// Zero-copy protocol: the decoder asks the sink for a destination buffer for
// the next n source rows via Buffer, writes the pixels there, then calls
// Commit with the same (y, n). For the common "no crop, no scale" case the
// returned buffer is a slice of the final image's Pix, so no copy happens.
package pipeline

import (
	"errors"
	"image"
)

// ErrStop is the recognizable signal with which a Sink asks the decoder to
// stop early. Return it (or an error wrapping it, see errors.Is) from
// Commit once the rows delivered so far are all the caller needs, or from
// Buffer to refuse the next batch before any of its pixels are written. A
// decoder that receives it stops at once and returns nil: it is not a
// failure, so it must not be confused with a corrupt stream, whose errors
// carry a status code such as Vp8StatusNotEnoughData. Rows committed before
// the signal stay final and are byte-identical to the prefix of a full
// decode; rows that were never committed are never written.
var ErrStop = errors.New("pipeline: stop decoding")

// Sink receives decoded rows.
type Sink interface {
	// Buffer returns a writable byte slice able to hold n full-width source
	// rows starting at source row y. Row r (0 <= r < n) starts at offset
	// r*Stride(). The slice is valid until the matching Commit call.
	// y must equal the y+n of the previous Commit (rows arrive in order).
	// A non-nil error rejects the batch before any of its pixels are
	// written: the decoder then stops without calling Commit for it.
	// ErrStop asks for a clean early stop (the decoder returns nil); any
	// other error aborts the decode with that error.
	Buffer(y, n int) ([]byte, error)
	// Stride returns the row stride, in bytes, of slices returned by Buffer.
	Stride() int
	// Commit marks rows [y, y+n) previously obtained from Buffer as final.
	// Returning ErrStop (or an error wrapping it) ends the decode cleanly
	// after these rows: the decoder returns nil and never writes another
	// row. Any other error aborts the decode with that error.
	Commit(y, n int) error
	// NeedRows reports how many source rows (from the top) the sink still
	// needs in total; decoders MAY stop after producing that many rows
	// (e.g. when cropping away the bottom of the image).
	NeedRows() int
}

// Direct writes rows straight into an *image.NRGBA whose bounds equal the
// source frame size (Min must be 0,0). It performs no copy.
type Direct struct {
	Img *image.NRGBA
}

// NewDirect returns a Direct sink for img.
func NewDirect(img *image.NRGBA) *Direct { return &Direct{Img: img} }

// Buffer implements Sink.
func (d *Direct) Buffer(y, n int) ([]byte, error) {
	s := d.Img.Stride
	end := (y + n) * s
	if end > len(d.Img.Pix) {
		end = len(d.Img.Pix)
	}
	return d.Img.Pix[y*s : end], nil
}

// Stride implements Sink.
func (d *Direct) Stride() int { return d.Img.Stride }

// Commit implements Sink.
func (d *Direct) Commit(y, n int) error { return nil }

// NeedRows implements Sink.
func (d *Direct) NeedRows() int { return d.Img.Rect.Dy() }
