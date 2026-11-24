package audio

import (
	"context"
	"errors"
	"io"
	"sync"

	"pipelined.dev/pipe"
	"pipelined.dev/pipe/mutable"
	"pipelined.dev/signal"
)

var (
	// ErrDifferentSampleRates is returned when signals with different
	// sample rates are sinked into mixer.
	ErrDifferentSampleRates = errors.New("sinking different sample rates")
	// ErrDifferentChannels is returned when signals with different number
	// of channels are sinked into mixer.
	ErrDifferentChannels = errors.New("sinking different channels")

	// errMixerClosed is returned when a sink is allocated after the mixer
	// source has terminated. The input is not registered with the mixer.
	errMixerClosed = errors.New("mixer is closed")
)

type (
	// Mixer summs up multiple signals. It has multiple sinks and a single
	// source.
	//
	// Inputs can be appended at runtime. Every input runs in its own sink
	// goroutine and hands mixed frames over to the source goroutine through
	// a pair of token channels. The mixer lock is only held for short
	// bookkeeping sections (snapshot, append, removal) and never while a
	// goroutine is blocked waiting for a frame, so appending an input can
	// not stall the source and vice versa.
	Mixer struct {
		initialize sync.Once
		sampleRate signal.Frequency
		channels   int
		pool       *signal.PoolAllocator
		// lock protects inputs and closed. It is never held while waiting
		// on an input, so adding inputs and reading frames can not block
		// each other.
		lock   sync.Mutex
		inputs []*mixerInput
		// closed is set once the source reached EOF or the pipe context
		// was canceled. Further sink allocations are rejected.
		closed bool
	}

	// mixerOutput represents a slice of samples to mix.
	mixerOutput struct {
		buffer signal.Floating
		len    int
	}

	// mixerInput is the hand-off state between a single sink goroutine and
	// the source goroutine.
	mixerInput struct {
		// write carries a single token when the sink is allowed to write
		// the next frame into buffer. The token is returned by the source
		// once the frame has been mixed.
		write chan struct{}
		// read carries a single token when a frame in buffer is ready for
		// the source.
		read chan struct{}
		// done is closed once the sink goroutine reached its flush hook.
		// After that no frame will ever arrive. It is the synchronization
		// point that makes it safe for the source to return buffer to the
		// pool.
		done chan struct{}
		// closeOnce guards closing done.
		closeOnce sync.Once
		// freeOnce guards the single return of buffer to the pool.
		freeOnce sync.Once
		// freed reports whether buffer was returned to the pool. It is set
		// inside freeOnce and may only be read after done closed or while
		// holding the mixer lock during teardown.
		freed  bool
		buffer signal.Floating
	}
)

func newMixerInput(buf signal.Floating) *mixerInput {
	write := make(chan struct{}, 1)
	write <- struct{}{}
	return &mixerInput{
		write:  write,
		read:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		buffer: buf,
	}
}

// signalDone closes the done channel exactly once, marking the sink as
// finished.
func (in *mixerInput) signalDone() {
	in.closeOnce.Do(func() { close(in.done) })
}

// freeBuffer returns the input buffer to the pool exactly once. It must be
// called only after the sink goroutine has signaled done, so no concurrent
// access to the buffer is possible.
func (in *mixerInput) freeBuffer(pool *signal.PoolAllocator) {
	in.freeOnce.Do(func() {
		in.freed = true
		in.buffer.Free(pool)
	})
}

// grantWrite hands the write token back to the sink. If the context is
// canceled before the token can be handed over, the sink is (or will soon
// be) exiting through its own context branch and does not need the token.
func (in *mixerInput) grantWrite(ctx context.Context) {
	select {
	case in.write <- struct{}{}:
	case <-ctx.Done():
	}
}

func (m *Mixer) init(sampleRate signal.Frequency, channels, bufferSize int) func() {
	return func() {
		m.channels = channels
		m.sampleRate = sampleRate
		m.pool = signal.GetPoolAllocator(channels, bufferSize, bufferSize)
	}
}

func mustAfterSink() {
	panic("mixer source bound before sink")
}

// Sink provides mixer sink allocator. Mixer sink receives a signal for
// mixing. Multiple sinks per mixer is allowed. Inputs with a different
// sample rate or number of channels are rejected and never become part of
// the mixing set.
func (m *Mixer) Sink() pipe.SinkAllocatorFunc {
	return func(mut mutable.Context, bufferSize int, props pipe.SignalProperties) (pipe.Sink, error) {
		m.initialize.Do(m.init(props.SampleRate, props.Channels, bufferSize))
		m.lock.Lock()
		defer m.lock.Unlock()
		if m.closed {
			return pipe.Sink{}, errMixerClosed
		}
		// Validate before allocating anything: a rejected input must not
		// leave a buffer behind and must not join the mixing set.
		if m.sampleRate != props.SampleRate {
			return pipe.Sink{}, ErrDifferentSampleRates
		}
		if m.channels != props.Channels {
			return pipe.Sink{}, ErrDifferentChannels
		}
		input := newMixerInput(m.pool.Float64())
		m.inputs = append(m.inputs, input)
		var sinkCtx context.Context
		return pipe.Sink{
			StartFunc: func(ctx context.Context) error {
				sinkCtx = ctx
				return nil
			},
			SinkFunc: func(floats signal.Floating) error {
				// Wait for the source to release the buffer. Both the
				// context branch and the token branch unblock promptly:
				// there is no busy waiting and no retry loop.
				select {
				case <-sinkCtx.Done():
					return nil
				case <-input.write:
				}
				n := signal.FloatingAsFloating(floats, input.buffer)
				if n != bufferSize {
					input.buffer = input.buffer.Slice(0, n)
				}
				select {
				case <-sinkCtx.Done():
					// The source is tearing down and will free the buffer
					// after observing done. Do not touch it anymore.
					return nil
				case input.read <- struct{}{}:
					return nil
				}
			},
			FlushFunc: func(ctx context.Context) error {
				// Signal completion instead of closing a data channel:
				// the source can now safely remove the input and return
				// its buffer, and a frame still pending in read is not
				// lost.
				input.signalDone()
				return nil
			},
		}, nil
	}
}

// Source provides mixer source allocator. Mixer source outputs mixed
// signal. Only single source per mixer is allowed. Must be called after
// Sink, otherwise will panic.
func (m *Mixer) Source() pipe.SourceAllocatorFunc {
	return func(mut mutable.Context, bufferSize int) (pipe.Source, error) {
		m.initialize.Do(mustAfterSink) // check that source is bound after sink.
		output := &mixerOutput{buffer: m.pool.Float64()}
		var sourceCtx context.Context
		return pipe.Source{
			SignalProperties: pipe.SignalProperties{
				Channels:   m.channels,
				SampleRate: m.sampleRate,
			},
			StartFunc: func(ctx context.Context) error {
				sourceCtx = ctx
				return nil
			},
			SourceFunc: func(out signal.Floating) (int, error) {
				for {
					// Snapshot the current inputs under the lock and do
					// all blocking waits lock-free, so runtime appends
					// never stall the mix loop.
					m.lock.Lock()
					if len(m.inputs) == 0 {
						m.closed = true
						m.lock.Unlock()
						return 0, io.EOF
					}
					inputs := make([]*mixerInput, len(m.inputs))
					copy(inputs, m.inputs)
					m.lock.Unlock()

					active := 0
					for _, input := range inputs {
						select {
						case <-sourceCtx.Done():
							// Flush hook performs the full teardown and
							// buffer reclamation.
							return 0, io.EOF
						case <-input.done:
							// The sink finished. A frame it produced just
							// before flushing may still be pending.
							select {
							case <-input.read:
								output.add(input.buffer)
								active++
								input.grantWrite(sourceCtx)
							default:
							}
							m.removeInput(input)
							input.freeBuffer(m.pool)
							continue
						case <-input.read:
						}
						output.add(input.buffer)
						active++
						input.grantWrite(sourceCtx)
					}
					if active == 0 {
						// Every input flushed this round; re-check the
						// mixing set instead of emitting an empty frame.
						continue
					}
					return output.sum(active, out) / m.channels, nil
				}
			},
			FlushFunc: func(ctx context.Context) error {
				m.teardown()
				output.buffer.Free(m.pool)
				return nil
			},
		}, nil
	}
}

// removeInput drops a finished input from the mixing set.
func (m *Mixer) removeInput(target *mixerInput) {
	m.lock.Lock()
	defer m.lock.Unlock()
	for i, input := range m.inputs {
		if input == target {
			m.inputs = append(m.inputs[:i], m.inputs[i+1:]...)
			return
		}
	}
}

// teardown marks the mixer closed and returns every still-allocated input
// buffer to the pool. It blocks only until each sink goroutine reaches its
// flush hook, which is guaranteed by the pipe lifecycle and always happens
// promptly because every sink blocking point honors its context.
func (m *Mixer) teardown() {
	m.lock.Lock()
	inputs := make([]*mixerInput, len(m.inputs))
	copy(inputs, m.inputs)
	m.closed = true
	m.lock.Unlock()

	for _, input := range inputs {
		<-input.done
		input.freeBuffer(m.pool)
	}

	m.lock.Lock()
	m.inputs = nil
	m.lock.Unlock()
}

// sum returns mixed samplein.
func (f *mixerOutput) sum(inputs int, out signal.Floating) (summed int) {
	for i := 0; i < f.buffer.Len(); i++ {
		out.SetSample(i, f.buffer.Sample(i)/float64(inputs))
		f.buffer.SetSample(i, 0)
	}
	summed, f.len = f.len, 0
	return
}

func (f *mixerOutput) add(in signal.Floating) {
	if f.len < in.Len() {
		f.len = in.Len()
	}

	for i := 0; i < in.Len(); i++ {
		f.buffer.SetSample(i, f.buffer.Sample(i)+in.Sample(i))
	}
	return
}
