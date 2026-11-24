package noisesuppressionstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/facebookincubator/go-belt/tool/logger"
	"github.com/iamcalledrob/circular"
	"github.com/xaionaro-go/audio/pkg/audio"
	"github.com/xaionaro-go/audio/pkg/noisesuppression"
	"github.com/xaionaro-go/observability"
)

const (
	debugBypassNoiseSuppression = false

	// defaultBufferSize is used when the caller does not specify a buffer
	// size.
	defaultBufferSize = 1024 * 1024

	// inputReadChunkSize is the maximum amount of PCM data read from the
	// upstream in a single call.
	inputReadChunkSize = 65536
)

// NoiseSuppressionStream is an io.Reader which applies noise suppression to
// the PCM stream of an upstream reader.
//
// Lifecycle:
//
//   - when the upstream returns io.EOF, the data which already got into the
//     input buffer is still noise-suppressed and delivered to the callers of
//     Read; only after the whole output is consumed Read returns io.EOF;
//   - a real error of the upstream or of the noise suppressor is returned by
//     Read instead of blocking forever;
//   - cancelling the context passed to NewNoiseSuppressionStream makes all
//     the pending Read calls and both background loops return in a finite
//     time (if the upstream implements io.Closer, it is closed to unblock a
//     Read which is stuck inside of it).
//
// Read is safe to call from multiple goroutines concurrently. Every call
// returns either (n > 0, nil) or (0, err); the terminal error (io.EOF, a real
// error or the context error) is sticky: once it is returned, all the
// subsequent calls return the same error.
type NoiseSuppressionStream struct {
	noisesuppression.NoiseSuppression
	encoding audio.Encoding
	channels audio.Channel

	// readCtx is the context of the consumer (passed to the constructor). It
	// is used to cancel the pending Read calls.
	readCtx context.Context

	// mu guards all the mutable fields below.
	mu sync.Mutex
	// notify is closed and replaced on every state change, which wakes up
	// everyone waiting for a state change (see waitLocked).
	notify chan struct{}

	inputBuffer  *circular.Buffer
	outputBuffer *circular.Buffer

	// readerDone is set when the upstream reached EOF and everything it
	// provided is already in the input buffer.
	readerDone bool
	// drained is set when the noise suppression loop consumed the whole
	// input buffer and put the result into the output buffer.
	drained bool

	// fatalErr is the first real (non-context, non-EOF) error of the
	// pipeline.
	fatalErr error
	// terminalErr is what Read returns from now on (io.EOF, fatalErr or the
	// context error).
	terminalErr error

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

var _ io.Reader = (*NoiseSuppressionStream)(nil)

func NewNoiseSuppressionStream(
	ctx context.Context,
	input io.Reader,
	noiseSuppression noisesuppression.NoiseSuppression,
	inputBufferSize uint,
	outputBufferSize uint,
) (*NoiseSuppressionStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	encoding, err := noiseSuppression.Encoding(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get the encoding of the noise suppression: %w", err)
	}
	channels, err := noiseSuppression.Channels(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get the amount of channels of the noise suppression: %w", err)
	}

	inputCap := int(inputBufferSize)
	outputCap := int(outputBufferSize)
	frameSize := int(noiseSuppression.ChunkSize())
	if inputCap <= 0 {
		inputCap = defaultBufferSize
	}
	if outputCap <= 0 {
		outputCap = defaultBufferSize
	}
	// A single frame must fit into both buffers, otherwise the pipeline
	// would deadlock on a buffer which is smaller than one frame.
	if frameSize > inputCap {
		inputCap = frameSize
	}
	if frameSize > outputCap {
		outputCap = frameSize
	}

	internalCtx, cancelFunc := context.WithCancel(ctx)
	s := &NoiseSuppressionStream{
		NoiseSuppression: noiseSuppression,
		encoding:         encoding,
		channels:         channels,
		readCtx:          ctx,
		cancel:           cancelFunc,
		notify:           make(chan struct{}),
		inputBuffer:      circular.NewBuffer(inputCap),
		outputBuffer:     circular.NewBuffer(outputCap),
	}

	// Two loops plus a goroutine which unblocks a Read stuck inside of the
	// upstream when the stream is being stopped.
	s.wg.Add(3)
	observability.Go(internalCtx, func(ctx context.Context) {
		defer s.wg.Done()
		err := s.readerLoop(ctx, input)
		s.mu.Lock()
		if err != nil {
			if ctx.Err() == nil {
				s.setFatalLocked(fmt.Errorf("got an error from the reader loop: %w", err))
			}
		} else {
			s.readerDone = true
		}
		s.notifyLocked()
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			// Stop the noise suppression loop as well: there is nothing to
			// drain and the readers have to receive the error.
			cancelFunc()
		}
	})
	observability.Go(internalCtx, func(ctx context.Context) {
		defer s.wg.Done()
		err := s.noiseSuppressionLoop(ctx)
		if err != nil {
			s.mu.Lock()
			if ctx.Err() == nil {
				s.setFatalLocked(fmt.Errorf("got an error from the noise suppression loop: %w", err))
			}
			s.notifyLocked()
			s.mu.Unlock()
		}
		// Once the noise suppression loop is over, the pipeline is over;
		// there is no point in keeping the other goroutines alive.
		cancelFunc()
	})

	// Unblock a Read which is stuck inside of the upstream when the stream is
	// being stopped (either canceled from the outside or failed).
	observability.Go(internalCtx, func(ctx context.Context) {
		defer s.wg.Done()
		<-ctx.Done()
		s.mu.Lock()
		alreadyDone := s.readerDone
		s.mu.Unlock()
		if alreadyDone {
			return
		}
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	})

	return s, nil
}

// waitBackground waits for the background goroutines to exit. It is mostly
// useful for tests, to be able to check that nothing is leaked.
func (s *NoiseSuppressionStream) waitBackground() {
	s.wg.Wait()
}

// setFatalLocked records the first real error of the pipeline.
//
// The mutex must be held.
func (s *NoiseSuppressionStream) setFatalLocked(err error) {
	if err == nil || s.fatalErr != nil {
		return
	}
	s.fatalErr = err
}

// notifyLocked wakes up everyone waiting for a state change.
//
// The mutex must be held.
func (s *NoiseSuppressionStream) notifyLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// abortReasonLocked returns the reason why the pipeline has to stop, if any.
//
// The mutex must be held.
func (s *NoiseSuppressionStream) abortReasonLocked(ctx context.Context) error {
	if s.fatalErr != nil {
		return s.fatalErr
	}
	return ctx.Err()
}

// waitLocked releases the mutex and waits for either a state change or the
// context cancellation, and acquires the mutex again. It returns false if the
// context was cancelled.
//
// The mutex must be held, and it is held again on return.
func (s *NoiseSuppressionStream) waitLocked(ctx context.Context) bool {
	ch := s.notify
	s.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-ch:
	}
	s.mu.Lock()
	return ctx.Err() == nil
}

func (s *NoiseSuppressionStream) readerLoop(
	ctx context.Context,
	input io.Reader,
) (_err error) {
	logger.Tracef(ctx, "readerLoop")
	defer func() { logger.Tracef(ctx, "/readerLoop %v", _err) }()

	readBuf := make([]byte, inputReadChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		logger.Tracef(ctx, "readerLoop: Read()")
		n, err := input.Read(readBuf)
		logger.Tracef(ctx, "/readerLoop: Read(): %v %v", n, err)
		if n < 0 {
			return fmt.Errorf("received invalid value of received bytes: %d", n)
		}
		if n > len(readBuf) {
			return fmt.Errorf("received an invalid amount of bytes: %d > %d", n, len(readBuf))
		}
		if n > 0 {
			s.mu.Lock()
			writeErr := s.writeInputLocked(ctx, readBuf[:n])
			s.mu.Unlock()
			if writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Not an error: the noise suppression loop still has to
				// drain the already buffered data.
				logger.Tracef(ctx, "readerLoop: reached EOF")
				return nil
			}
			return fmt.Errorf("unable to read the backend: %w", err)
		}
	}
}

// writeInputLocked writes p to the input buffer, blocking while the buffer is
// full.
//
// The mutex must be held, and it is held again on return.
func (s *NoiseSuppressionStream) writeInputLocked(
	ctx context.Context,
	p []byte,
) error {
	for len(p) > 0 {
		if err := s.abortReasonLocked(ctx); err != nil {
			return err
		}
		n, err := s.inputBuffer.Write(p)
		if n > 0 {
			p = p[n:]
			s.notifyLocked()
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, circular.ErrNoSpace) {
			return fmt.Errorf("unable to write to the circular buffer: %w", err)
		}
		if !s.waitLocked(ctx) {
			return s.abortReasonLocked(ctx)
		}
	}
	return nil
}

// writeOutputLocked writes p to the output buffer, blocking while the buffer
// is full (i.e. while the consumer is not reading).
//
// The mutex must be held, and it is held again on return.
func (s *NoiseSuppressionStream) writeOutputLocked(
	ctx context.Context,
	p []byte,
) error {
	for len(p) > 0 {
		if err := s.abortReasonLocked(ctx); err != nil {
			return err
		}
		n, err := s.outputBuffer.Write(p)
		if n > 0 {
			p = p[n:]
			s.notifyLocked()
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, circular.ErrNoSpace) {
			return fmt.Errorf("unable to write to the circular buffer: %w", err)
		}
		if !s.waitLocked(ctx) {
			return s.abortReasonLocked(ctx)
		}
	}
	return nil
}

func (s *NoiseSuppressionStream) noiseSuppressionLoop(ctx context.Context) (_err error) {
	logger.Tracef(ctx, "noiseSuppressionLoop")
	defer func() { logger.Tracef(ctx, "/noiseSuppressionLoop: %v", _err) }()

	frameSize := int(s.ChunkSize())
	logger.Debugf(ctx, "frameSize: %d", frameSize)

	var (
		frameInput  []byte
		frameOutput []byte
		// frameLen is the size of the frame being accumulated, or 0 when the
		// next frame is not started yet.
		frameLen int
		// receivedCount is how many bytes of the current frame are already
		// accumulated in frameInput (it survives the waits for more data).
		receivedCount int
	)
	for {
		s.mu.Lock()
		if err := s.abortReasonLocked(ctx); err != nil {
			s.mu.Unlock()
			// The reader loop reports the reason to the consumer.
			return nil
		}
		if receivedCount == 0 && s.readerDone && s.inputBuffer.Len() == 0 {
			// Everything that the upstream provided is processed and queued
			// into the output buffer; nothing will ever be added.
			s.drained = true
			s.notifyLocked()
			s.mu.Unlock()
			return nil
		}

		if frameLen == 0 {
			if frameSize > 0 {
				frameLen = frameSize
			} else {
				// The noise suppressor does not define a frame size: process
				// whatever is available as a single block.
				frameLen = s.inputBuffer.Len()
				if frameLen == 0 {
					if s.readerDone {
						s.drained = true
						s.notifyLocked()
						s.mu.Unlock()
						return nil
					}
					s.waitLocked(ctx)
					s.mu.Unlock()
					continue
				}
			}
			if cap(frameInput) < frameLen {
				frameInput = make([]byte, frameLen)
				frameOutput = make([]byte, frameLen)
			}
			frameInput = frameInput[:frameLen]
			frameOutput = frameOutput[:frameLen]
		}

		for receivedCount < frameLen {
			n, err := s.inputBuffer.Read(frameInput[receivedCount:])
			if n > 0 {
				receivedCount += n
				s.notifyLocked()
			}
			if err == nil {
				continue
			}
			if !errors.Is(err, io.EOF) {
				s.mu.Unlock()
				return fmt.Errorf("unable to read from the circular buffer: %w", err)
			}
			break
		}

		if receivedCount < frameLen {
			if !s.readerDone {
				// The upstream may still provide the rest of the frame.
				if !s.waitLocked(ctx) {
					s.mu.Unlock()
					return nil
				}
				s.mu.Unlock()
				continue
			}
			// The upstream is over; process the tail of the stream as a
			// zero-padded frame, but deliver only the bytes which really came
			// from the upstream.
			clear(frameInput[receivedCount:frameLen])
		}

		validLen := receivedCount
		frameLen = 0
		receivedCount = 0
		s.mu.Unlock()

		if debugBypassNoiseSuppression {
			time.Sleep(time.Millisecond)
			copy(frameOutput[:validLen], frameInput[:validLen])
		} else if frameSize == 0 {
			// The noise suppressor does not define a frame size and (usually)
			// does not process anything; just pass the data through.
			copy(frameOutput[:validLen], frameInput[:validLen])
		} else {
			logger.Tracef(ctx, "s.NoiseSuppression.SuppressNoise")
			_, err := s.NoiseSuppression.SuppressNoise(ctx, frameInput, frameOutput)
			logger.Tracef(ctx, "/s.NoiseSuppression.SuppressNoise: %v", err)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("unable to noise-suppress: %w", err)
			}
		}

		s.mu.Lock()
		err := s.writeOutputLocked(ctx, frameOutput[:validLen])
		s.mu.Unlock()
		if err != nil {
			return nil
		}
	}
}

// readOutputLocked copies as much data as possible from the output buffer. A
// non-zero return value is always accompanied by a nil error, so that a caller
// never receives both data and an error.
//
// The mutex must be held.
func (s *NoiseSuppressionStream) readOutputLocked(pcm []byte) (int, error) {
	n, err := s.outputBuffer.Read(pcm)
	if n > 0 {
		s.notifyLocked()
		return n, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return 0, nil
	}
	return 0, fmt.Errorf("unable to read from the circular buffer: %w", err)
}

func (s *NoiseSuppressionStream) Read(pcm []byte) (_ret int, _err error) {
	logger.Tracef(s.readCtx, "Read, len:%d", len(pcm))
	defer func() { logger.Tracef(s.readCtx, "/Read, len:%d: %d, %v", len(pcm), _ret, _err) }()

	if len(pcm) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		// A determined result is sticky.
		if s.terminalErr != nil {
			return 0, s.terminalErr
		}
		if s.fatalErr != nil {
			s.terminalErr = s.fatalErr
			return 0, s.terminalErr
		}

		if s.drained {
			// The whole stream is already noise-suppressed. Deliver what is
			// left and only then report the end of the stream. It wins over a
			// concurrently cancelled context: the result is either "the
			// complete data followed by io.EOF" or "the context error".
			n, err := s.readOutputLocked(pcm)
			if err != nil {
				s.terminalErr = err
				return 0, err
			}
			if n > 0 {
				return n, nil
			}
			s.terminalErr = io.EOF
			return 0, io.EOF
		}

		if err := s.readCtx.Err(); err != nil {
			// Wake up the background loops, they may currently be waiting for
			// the consumer.
			s.terminalErr = err
			s.cancel()
			s.notifyLocked()
			return 0, err
		}

		n, err := s.readOutputLocked(pcm)
		if err != nil {
			s.terminalErr = err
			return 0, err
		}
		if n > 0 {
			return n, nil
		}

		s.waitLocked(s.readCtx)
	}
}
