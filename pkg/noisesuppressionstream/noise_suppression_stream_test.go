package noisesuppressionstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xaionaro-go/audio/pkg/audio"
)

// fakeSuppressor is a controllable noisesuppression.NoiseSuppression, which
// applies a byte-wise transformation to the data it processes.
type fakeSuppressor struct {
	chunkSize uint

	mu        sync.Mutex
	calls     int
	err       error
	delay     time.Duration
	transform func(dst, src []byte)
}

var _ interface {
	audio.AbstractAnalyzer
	ChunkSize() uint
	SuppressNoise(context.Context, []byte, []byte) (float64, error)
} = (*fakeSuppressor)(nil)

func newFakeSuppressor(chunkSize uint) *fakeSuppressor {
	return &fakeSuppressor{chunkSize: chunkSize}
}

func (s *fakeSuppressor) Close() error { return nil }

func (s *fakeSuppressor) Encoding(context.Context) (audio.Encoding, error) {
	return audio.EncodingPCM{PCMFormat: audio.PCMFormatS16LE, SampleRate: 48000}, nil
}

func (s *fakeSuppressor) Channels(context.Context) (audio.Channel, error) {
	return 1, nil
}

func (s *fakeSuppressor) ChunkSize() uint { return s.chunkSize }

func (s *fakeSuppressor) SuppressNoise(
	ctx context.Context,
	input []byte,
	output []byte,
) (float64, error) {
	s.mu.Lock()
	s.calls++
	delay := s.delay
	err := s.err
	transform := s.transform
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err != nil {
		return 0, err
	}
	if transform != nil {
		transform(output, input)
	} else {
		copy(output, input)
	}
	return 1, nil
}

func (s *fakeSuppressor) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeSuppressor) setTransform(fn func(dst, src []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transform = fn
}

func (s *fakeSuppressor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// sequenceReader serves a predefined sequence of data and then either io.EOF
// or a configured error, in chunks of at most maxChunk bytes.
type sequenceReader struct {
	data     []byte
	maxChunk int
	err      error
	closed   bool

	mu     sync.Mutex
	offset int
}

func newSequenceReader(data []byte, maxChunk int, err error) *sequenceReader {
	return &sequenceReader{data: data, maxChunk: maxChunk, err: err}
}

func (r *sequenceReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errors.New("read from a closed reader")
	}
	if r.offset >= len(r.data) {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := len(p)
	if rest := len(r.data) - r.offset; n > rest {
		n = rest
	}
	if r.maxChunk > 0 && n > r.maxChunk {
		n = r.maxChunk
	}
	n = copy(p, r.data[r.offset:r.offset+n])
	r.offset += n
	return n, nil
}

func (r *sequenceReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// blockingReader never returns any data until it is closed.
type blockingReader struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReader() *blockingReader {
	return &blockingReader{closed: make(chan struct{})}
}

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("blocked reader was closed")
}

func (r *blockingReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// isDrained reports whether the whole upstream data is already noise-suppressed
// and is queued to the output buffer.
func (s *NoiseSuppressionStream) isDrained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drained
}

func waitFor(
	t *testing.T,
	what string,
	cond func() bool,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitBackgroundWithTimeout(
	t *testing.T,
	s *NoiseSuppressionStream,
) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.waitBackground()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the background goroutines did not exit")
	}
}

// readAllWithTimeout reads until an error, with a hard timeout, so a broken
// implementation fails the test instead of hanging it.
func readAllWithTimeout(
	t *testing.T,
	r io.Reader,
	bufSize int,
	timeout time.Duration,
) ([]byte, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		var data []byte
		buf := make([]byte, bufSize)
		for {
			n, err := r.Read(buf)
			if n < 0 || n > len(buf) {
				resultCh <- result{data, fmt.Errorf("invalid Read result: %d", n)}
				return
			}
			if n > 0 {
				if err != nil {
					resultCh <- result{data, fmt.Errorf("Read returned both data and an error: %d, %v", n, err)}
					return
				}
				data = append(data, buf[:n]...)
				continue
			}
			if err != nil {
				resultCh <- result{data, err}
				return
			}
		}
	}()
	select {
	case res := <-resultCh:
		return res.data, res.err
	case <-time.After(timeout):
		t.Fatal("Read did not return in time (blocked forever)")
		return nil, nil
	}
}

func incrementTransform(dst, src []byte) {
	for i := range src {
		dst[i] = src[i] + 1
	}
}

func expectedTransformed(src []byte) []byte {
	out := make([]byte, len(src))
	for i := range src {
		out[i] = src[i] + 1
	}
	return out
}

func testData(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

// pseudoRandomData returns a deterministic non-periodic byte sequence. It is
// used to detect a lost or a duplicated chunk by comparing byte histograms.
func pseudoRandomData(n int) []byte {
	data := make([]byte, n)
	var state uint64 = 0x9E3779B97F4A7C15
	for i := range data {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		data[i] = byte(state)
	}
	return data
}

func byteHistogram(data []byte) [256]int {
	var hist [256]int
	for _, b := range data {
		hist[b]++
	}
	return hist
}

// TestDrainAfterEOF checks that the data which already got into the buffers is
// noise-suppressed and delivered completely before io.EOF is returned, even if
// the upstream sends the data in tiny chunks and the buffers are small.
func TestDrainAfterEOF(t *testing.T) {
	const (
		dataSize    = 1000
		frameSize   = 16
		inputBuffer = 8 // deliberately smaller than a frame
		outBuffer   = 4096
	)
	data := testData(dataSize)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 7, nil),
		suppressor,
		inputBuffer,
		outBuffer,
	)
	require.NoError(t, err)

	// The consumer is slow: it starts reading only after the upstream has
	// completed, so the whole stream is buffered/backpressured first.
	waitFor(t, "the stream to be drained", s.isDrained)

	got, err := readAllWithTimeout(t, s, 13, 10*time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, expectedTransformed(data), got)
	require.Positive(t, suppressor.callCount())

	// The result is stable.
	n, err := s.Read(make([]byte, 16))
	require.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
	n, err = s.Read(make([]byte, 16))
	require.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)

	waitBackgroundWithTimeout(t, s)
}

// TestEmptyUpstream checks that an upstream with no data at all results in
// a prompt io.EOF.
func TestEmptyUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(nil, 0, nil),
		newFakeSuppressor(16),
		1024,
		1024,
	)
	require.NoError(t, err)

	got, err := readAllWithTimeout(t, s, 16, 10*time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Empty(t, got)

	waitBackgroundWithTimeout(t, s)
}

// TestPartialFrameOnEOF checks that the tail which does not form a complete
// frame is still delivered (and only the bytes which really came from the
// upstream).
func TestPartialFrameOnEOF(t *testing.T) {
	const frameSize = 16
	data := testData(frameSize*3 + 5)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 3, nil),
		suppressor,
		1024,
		1024,
	)
	require.NoError(t, err)

	got, err := readAllWithTimeout(t, s, 7, 10*time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, expectedTransformed(data), got)

	waitBackgroundWithTimeout(t, s)
}

// TestUpstreamError checks that a real error of the upstream is returned by
// Read instead of being treated as EOF (or blocking forever), and that it is
// stable.
func TestUpstreamError(t *testing.T) {
	upstreamErr := errors.New("upstream failure")
	data := testData(100)
	suppressor := newFakeSuppressor(16)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 8, upstreamErr),
		suppressor,
		1024,
		1024,
	)
	require.NoError(t, err)

	got, err := readAllWithTimeout(t, s, 16, 10*time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, upstreamErr)
	require.NotErrorIs(t, err, io.EOF)
	for _, b := range got {
		// Everything delivered has to be processed data.
		require.Contains(t, expectedTransformed(data), b)
	}

	// Same error on every subsequent call.
	for i := 0; i < 3; i++ {
		n, err2 := s.Read(make([]byte, 16))
		require.Zero(t, n)
		require.Equal(t, err, err2)
	}

	waitBackgroundWithTimeout(t, s)
}

// TestSuppressorError checks that a real error of the noise suppressor is
// returned by Read.
func TestSuppressorError(t *testing.T) {
	suppressorErr := errors.New("noise suppression failure")
	suppressor := newFakeSuppressor(16)
	suppressor.setError(suppressorErr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(testData(1024), 64, nil),
		suppressor,
		1024,
		1024,
	)
	require.NoError(t, err)

	_, err = readAllWithTimeout(t, s, 16, 10*time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, suppressorErr)
	require.NotErrorIs(t, err, io.EOF)

	n, err2 := s.Read(make([]byte, 16))
	require.Zero(t, n)
	require.ErrorIs(t, err2, suppressorErr)

	waitBackgroundWithTimeout(t, s)
}

// TestCancelUnblocksRead checks that cancelling the context unblocks a Read
// which waits for data (the upstream provides nothing at all) and stops all
// the background goroutines.
func TestCancelUnblocksRead(t *testing.T) {
	suppressor := newFakeSuppressor(16)
	upstream := newBlockingReader()

	ctx, cancel := context.WithCancel(context.Background())

	s, err := NewNoiseSuppressionStream(ctx, upstream, suppressor, 1024, 1024)
	require.NoError(t, err)

	readErrCh := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 1024))
		readErrCh <- err
	}()

	// Give the Read a chance to start waiting.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-readErrCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Read did not return after the context was cancelled")
	}

	waitBackgroundWithTimeout(t, s)

	// Every subsequent Read returns the same error.
	n, err := s.Read(make([]byte, 16))
	require.Zero(t, n)
	require.ErrorIs(t, err, context.Canceled)
}

// TestCancelWhileOutputBufferIsFull checks that cancelling unblocks the noise
// suppression loop when the output buffer is full and nobody reads, and that
// nothing is left waiting on a buffer.
func TestCancelWhileOutputBufferIsFull(t *testing.T) {
	const (
		frameSize  = 16
		bufferSize = 64
	)
	suppressor := newFakeSuppressor(frameSize)
	upstream := newSequenceReader(testData(1024*1024), 1024, nil)

	ctx, cancel := context.WithCancel(context.Background())

	s, err := NewNoiseSuppressionStream(ctx, upstream, suppressor, bufferSize, bufferSize)
	require.NoError(t, err)

	// Let the pipeline fill up the buffers and block.
	time.Sleep(100 * time.Millisecond)
	cancel()

	waitBackgroundWithTimeout(t, s)

	got, err := readAllWithTimeout(t, s, frameSize, 10*time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.LessOrEqual(t, len(got), bufferSize)
}

// TestCancelAfterDrainDeliversEverything checks the resolution of the race
// between a normal end and a cancellation: once the whole stream is
// noise-suppressed, Read delivers all the data and only then reports io.EOF,
// even if the context is cancelled in the meantime.
func TestCancelAfterDrainDeliversEverything(t *testing.T) {
	const frameSize = 16
	data := testData(1000)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 32, nil),
		suppressor,
		1024,
		2048,
	)
	require.NoError(t, err)

	waitFor(t, "the stream to be drained", s.isDrained)
	// Cancel after the whole data is already processed, but before it is read.
	cancel()

	got, err := readAllWithTimeout(t, s, 64, 10*time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, expectedTransformed(data), got)

	waitBackgroundWithTimeout(t, s)
}

// TestConcurrentReads checks that concurrent Read calls neither lose nor
// duplicate data and that every call returns either data or an error (never
// both), and that the terminal error is stable for all readers.
//
// The order in which the concurrent readers complete is not deterministic, so
// the whole delivered stream can be compared to the expected one only as a
// multiset: a lost chunk or a duplicated chunk changes the byte histogram.
func TestConcurrentReads(t *testing.T) {
	const (
		frameSize   = 16
		dataSize    = 100_000
		readerCount = 8
	)
	data := pseudoRandomData(dataSize)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 333, nil),
		suppressor,
		1024,
		1024,
	)
	require.NoError(t, err)

	var (
		wg       sync.WaitGroup
		resultMu sync.Mutex
		got      []byte
		readErrs []error
	)
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 1+i*17)
			for {
				n, err := s.Read(buf)
				if n < 0 || n > len(buf) {
					resultMu.Lock()
					readErrs = append(readErrs, fmt.Errorf("invalid Read result: %d", n))
					resultMu.Unlock()
					return
				}
				if n > 0 {
					if err != nil {
						resultMu.Lock()
						readErrs = append(readErrs, fmt.Errorf("Read returned both data and an error: %d, %v", n, err))
						resultMu.Unlock()
						return
					}
					resultMu.Lock()
					got = append(got, buf[:n]...)
					resultMu.Unlock()
					continue
				}
				if err != nil {
					resultMu.Lock()
					readErrs = append(readErrs, err)
					resultMu.Unlock()
					return
				}
			}
		}(i)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent readers did not finish")
	}

	require.NotEmpty(t, readErrs)
	// Every reader has to observe the same terminal error.
	for _, err := range readErrs {
		require.ErrorIs(t, err, io.EOF)
	}
	require.Equal(t, byteHistogram(expectedTransformed(data)), byteHistogram(got))
	require.Len(t, got, len(data))

	waitBackgroundWithTimeout(t, s)
}

// TestConcurrentReadsWithCancel checks that a cancellation wakes up all the
// concurrently waiting readers.
func TestConcurrentReadsWithCancel(t *testing.T) {
	const readerCount = 8
	suppressor := newFakeSuppressor(16)
	upstream := newBlockingReader()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewNoiseSuppressionStream(ctx, upstream, suppressor, 1024, 1024)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errCh := make(chan error, readerCount)
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Read(make([]byte, 128))
			errCh <- err
		}()
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the readers did not return after the context was cancelled")
	}
	close(errCh)
	for err := range errCh {
		require.ErrorIs(t, err, context.Canceled)
	}

	waitBackgroundWithTimeout(t, s)
}

// TestNoDuplicateDelivery checks that the same bytes are never delivered twice
// even when the reader reads in chunks smaller than the output buffer.
func TestNoDuplicateDelivery(t *testing.T) {
	const frameSize = 16
	data := testData(5000)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 1, nil),
		suppressor,
		128,
		128,
	)
	require.NoError(t, err)

	var got []byte
	buf := make([]byte, 3)
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, err := s.Read(buf)
		require.GreaterOrEqual(t, n, 0)
		if n > 0 {
			require.NoError(t, err)
			got = append(got, buf[:n]...)
		}
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
		require.False(t, time.Now().After(deadline), "the stream did not finish in time")
	}
	require.Equal(t, expectedTransformed(data), got)

	waitBackgroundWithTimeout(t, s)
}

// TestZeroBufferSizes checks that the stream works even if the caller does not
// specify the buffer sizes.
func TestZeroBufferSizes(t *testing.T) {
	const frameSize = 16
	data := testData(5000)
	suppressor := newFakeSuppressor(frameSize)
	suppressor.setTransform(incrementTransform)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewNoiseSuppressionStream(
		ctx,
		newSequenceReader(data, 100, nil),
		suppressor,
		0,
		0,
	)
	require.NoError(t, err)

	got, err := readAllWithTimeout(t, s, 4096, 10*time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, expectedTransformed(data), got)

	waitBackgroundWithTimeout(t, s)
}
