package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
)

// NumberType represents a PCM number type.
type NumberType int

// Possible NumberTypes
const (
	Int8 = iota
	Int16
	Int32
	Float32
)

// ErrInvalidNumberType is returned if an invalid number type is provided,
// mainly to offline streams.
var ErrInvalidNumberType = errors.New("audio: invalid number type")

// ErrOfflineStreamClosed is returned by writes performed on an OfflineStream
// that has already been closed. Such writes never modify the buffered data.
var ErrOfflineStreamClosed = errors.New("audio: attempt to write to closed OfflineStream")

// OfflineStream represents a stream which is wrapped and is
// usually based on a different stream of bytes or numbers, such as
// over a network.
//
// An OfflineStream is safe for concurrent use by multiple writers and
// readers. Every buffered sample is delivered to exactly one reader.
type OfflineStream struct {
	Stream
	mu         sync.Mutex
	cond       *sync.Cond
	buffer     []int32
	sampleRate int
	// closed is set once the stream will never receive more data. It is set
	// by Close, or implicitly when ReadBytes reaches the end of its source
	// (including when the source returns an error).
	closed bool
	// err holds the terminal error that finished the stream, if any. A nil
	// err with closed == true means a graceful close: readers drain the
	// buffer and then receive io.EOF.
	err        error
	bufferSize int
}

// NewOfflineStream returns an offline stream, a simple buffered stream that
// allows you to convert io.Read/Write operations into a stream. Buffer size
// is measured by number of samples.
func NewOfflineStream(sampleRate int, bufferSize int) *OfflineStream {
	newStream := &OfflineStream{
		sampleRate: sampleRate,
		bufferSize: bufferSize,
	}
	newStream.cond = sync.NewCond(&newStream.mu)

	return newStream
}

// ReadBytes reads bytes of values into the offline stream. It blocks until
// the source reader returns an error (including io.EOF), at which point the
// stream is finished and waiting readers are woken up: any already buffered
// data can still be drained, after which readers receive the same error.
//
// If the stream is closed before or while ReadBytes is running,
// ErrOfflineStreamClosed is returned and no further data is appended.
func (o *OfflineStream) ReadBytes(rd io.Reader, bo binary.ByteOrder, numType NumberType) error {
	if err := o.checkOpen(); err != nil {
		return err
	}

	var byteBufferSize int
	switch numType {
	case Int8:
		byteBufferSize = o.bufferSize
	case Int16:
		byteBufferSize = 2 * o.bufferSize
	case Int32, Float32:
		byteBufferSize = 4 * o.bufferSize
	default:
		return ErrInvalidNumberType
	}

	buffer := make([]byte, byteBufferSize)
	for {
		n, err := rd.Read(buffer)
		if n > 0 {
			samples, convErr := convertByteSamples(numType, bo, buffer[:n])
			if convErr != nil {
				return convErr
			}
			if appendErr := o.appendSamples(samples); appendErr != nil {
				return appendErr
			}
		}

		if err != nil {
			// Finish the stream with the upstream error so that waiting
			// readers stop blocking and eventually observe it. io.EOF is
			// preserved, giving readers a normal end-of-stream after they
			// have drained everything already enqueued.
			o.finish(err)
			return err
		}
	}
}

// convertByteSamples decodes one chunk of PCM bytes into int32 samples,
// applying the same scaling rules as the ReadFrom* conversion helpers.
func convertByteSamples(numType NumberType, bo binary.ByteOrder, buffer []byte) ([]int32, error) {
	switch numType {
	case Int8:
		middle := make([]int8, len(buffer))
		for i := 0; i < len(buffer); i++ {
			middle[i] = int8(buffer[i])
		}
		out := make([]int32, len(middle))
		ReadFromInt8(out, middle, len(middle))
		return out, nil
	case Int16:
		n := len(buffer) / 2
		middle := make([]int16, n)
		for i := 0; i < n; i++ {
			middle[i] = int16(bo.Uint16(buffer[i*2:]))
		}
		out := make([]int32, n)
		ReadFromInt16(out, middle, n)
		return out, nil
	case Int32:
		n := len(buffer) / 4
		out := make([]int32, n)
		for i := 0; i < n; i++ {
			out[i] = int32(bo.Uint32(buffer[i*4:]))
		}
		return out, nil
	case Float32:
		n := len(buffer) / 4
		middle := make([]float32, n)
		for i := 0; i < n; i++ {
			middle[i] = math.Float32frombits(bo.Uint32(buffer[i*4:]))
		}
		out := make([]int32, n)
		ReadFromFloat32(out, middle, n)
		return out, nil
	default:
		return nil, ErrInvalidNumberType
	}
}

// WriteBytes writes bytes of values into the offline stream. Reaching the
// end of b is a successful write and returns nil (the underlying io.EOF is
// hidden, matching write rather than read semantics).
func (o *OfflineStream) WriteBytes(b []byte, bo binary.ByteOrder, numType NumberType) error {
	rd := bytes.NewReader(b)
	if err := o.ReadBytes(rd, bo, numType); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// WriteValues writes a slice of number values into the offline stream. If
// the stream has been closed, ErrOfflineStreamClosed is returned and the
// buffer is left untouched.
func (o *OfflineStream) WriteValues(val interface{}) error {
	length := SliceLength(val)
	result := make([]int32, length)
	if err := ReadFromAnything(result, val, length); err != nil {
		return err
	}

	return o.appendSamples(result)
}

// appendSamples appends samples atomically with respect to Close: the
// closed check and the buffer mutation happen under the same lock, so no
// write can ever land in a closed stream's buffer.
func (o *OfflineStream) appendSamples(samples []int32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrOfflineStreamClosed
	}
	o.buffer = append(o.buffer, samples...)
	// Wake every blocked reader; sync.Cond cannot lose a wakeup because
	// waiting readers re-check the state under this same lock.
	o.cond.Broadcast()
	return nil
}

// finish marks the stream as closed with a terminal error and wakes all
// waiters. It is used when an upstream source ends.
func (o *OfflineStream) finish(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		o.err = err
	}
	o.cond.Broadcast()
}

func (o *OfflineStream) checkOpen() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrOfflineStreamClosed
	}
	return nil
}

// Close closes the offline stream, and causes any ongoing reads to return
// once the buffered data has been drained. Closing an already closed stream
// is safe and has no additional effect.
func (o *OfflineStream) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	o.cond.Broadcast()
}

// Read reads samples into dst. It blocks until enough samples are available
// or the stream finishes. On a finished stream, any remaining buffered
// samples are returned first (possibly as a partial read); once the buffer
// is empty, the terminal error is returned, which is io.EOF for a graceful
// close. Concurrent reads each consume distinct samples: no sample is
// delivered twice or lost.
func (o *OfflineStream) Read(dst interface{}) (int, error) {
	length := SliceLength(dst)

	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if length > 0 && len(o.buffer) >= length {
			if err := ReadFromInt32(dst, o.buffer, length); err != nil {
				return 0, err
			}
			o.buffer = o.buffer[length:]
			return length, nil
		}

		if o.closed {
			// Drain whatever was enqueued before the stream finished,
			// in order, before reporting EOF or the terminal error.
			if len(o.buffer) > 0 {
				last := len(o.buffer)
				if err := ReadFromInt32(dst, o.buffer, last); err != nil {
					return 0, err
				}
				o.buffer = nil
				return last, nil
			}
			if o.err != nil {
				return 0, o.err
			}
			return 0, io.EOF
		}

		if length == 0 {
			// Nothing can ever be copied into dst; mirror the conversion
			// helper's verdict instead of blocking forever.
			return 0, ReadFromInt32(dst, nil, 0)
		}

		// Atomically releases the lock and suspends; Broadcast calls are
		// never lost, unlike signals on an unbuffered channel.
		o.cond.Wait()
	}
}

// SampleRate returns the sample rate of the offline stream.
func (o *OfflineStream) SampleRate() int {
	return o.sampleRate
}
