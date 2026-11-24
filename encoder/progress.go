package encoder

import (
	"context"
	"sync"
	"time"

	"github.com/farshidrezaei/mosaic/internal/executor"
)

// runEncodingCommand executes an FFmpeg command while streaming progress blocks
// to handler.
//
// The function guarantees exactly one terminal outcome:
//
//   - Success: every progress update (including the final "progress=end" block)
//     has been handed to handler before the function returns, and the executor
//     usage is returned.
//   - Cancellation: ctx.Err() is returned promptly, no further handler call is
//     started, and the function never blocks waiting for a slow or stuck handler.
//   - Command failure: the executor error (which may carry command/stderr
//     context) is returned unchanged.
//
// Progress is parsed and delivered on dedicated goroutines, so a slow handler
// cannot stall the executor's pipe reader or deadlock cancellation. On success
// the delivery pipeline is drained and joined, which is what preserves the
// final progress event.
func runEncodingCommand(
	ctx context.Context,
	exec executor.CommandExecutor,
	handler func(map[string]string),
	name string,
	args []string,
) (*executor.Usage, error) {
	if handler == nil {
		_, usage, err := exec.Execute(ctx, name, args...)
		return usage, err
	}

	progressChan := make(chan string)

	type commandResult struct {
		usage *executor.Usage
		err   error
	}
	resultCh := make(chan commandResult, 1)

	go func() {
		_, usage, err := exec.ExecuteWithProgress(ctx, progressChan, name, args...)
		resultCh <- commandResult{usage: usage, err: err}
	}()

	pipe := newProgressPipe(ctx, handler)
	go pipe.consume(progressChan)

	select {
	case res := <-resultCh:
		// The executor has returned, so it will send no more progress.
		pipe.producerFinished()
		// Cancellation wins even if it raced with command completion: the
		// caller asked for the work to stop, so report it as cancelled.
		if ctxErr := ctx.Err(); ctxErr != nil {
			pipe.stop()
			return nil, ctxErr
		}
		if res.err != nil {
			pipe.stop()
			return nil, res.err
		}
		// Success: drain and join the progress pipeline so the last block is
		// delivered. A slow handler cannot prevent cancellation from returning.
		if !pipe.wait(ctx) {
			return nil, ctx.Err()
		}
		return res.usage, nil
	case <-ctx.Done():
		// Do not wait for the delivery goroutine: the handler may be blocked,
		// and the caller needs the cancellation to return promptly. stop drops
		// queued updates and prevents new callbacks. The consumer keeps
		// draining until the executor closes the channel, so a still-running
		// executor never blocks writing progress.
		pipe.stop()
		// Give a context-aware executor a brief moment to reap its process.
		// This lets the caller delete temporary input files without racing a
		// still-open file handle, while keeping cancellation bounded even if an
		// executor ignores the context.
		select {
		case <-resultCh:
			pipe.producerFinished()
		case <-time.After(cancelReapGrace):
			// Executor did not observe the cancellation in time. Retire the
			// consumer whenever it eventually returns so it does not leak.
			go func() {
				<-resultCh
				pipe.producerFinished()
			}()
		}
		return nil, ctx.Err()
	}
}

// cancelReapGrace bounds how long cancellation waits for the executor to
// return after the context is done. It is not a handler timeout.
const cancelReapGrace = 250 * time.Millisecond

// progressPipe decouples progress parsing from callback delivery.
//
// The consumer goroutine only enqueues parsed snapshots under a mutex, so it
// never blocks on a slow handler and always keeps draining the executor pipe.
// A single delivery goroutine invokes handler sequentially, preserving the
// original ordering guarantee.
//
// stop() is the only way to end delivery early. Because handler can block, it
// is invoked without holding the lock; a callback already claimed before stop
// may finish, but no callback is claimed after stop is observed.
type progressPipe struct {
	ctx     context.Context
	handler func(map[string]string)

	mu       sync.Mutex
	cond     *sync.Cond
	pending  []map[string]string
	closed   bool // consume finished: no more snapshots can arrive
	stopped  bool // terminal: discard everything and stop delivering
	finished chan struct{}

	producerDone chan struct{}
	producerOnce sync.Once
}

func newProgressPipe(ctx context.Context, handler func(map[string]string)) *progressPipe {
	p := &progressPipe{
		ctx:          ctx,
		handler:      handler,
		finished:     make(chan struct{}),
		producerDone: make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
	go p.deliver()
	return p
}

// producerFinished signals that the executor has returned and will send no
// further progress. It lets consume terminate even if an executor does not
// close the progress channel.
func (p *progressPipe) producerFinished() {
	p.producerOnce.Do(func() { close(p.producerDone) })
}

// consume parses progress blocks from ch until it is closed or the producer is
// known to be finished. It keeps draining after stop so the executor is never
// blocked writing progress.
func (p *progressPipe) consume(ch <-chan string) {
	acc := newProgressAccumulator()

	read := func(raw string) {
		for _, snapshot := range acc.add(raw) {
			p.push(snapshot)
		}
	}

Loop:
	for {
		select {
		case raw, ok := <-ch:
			if !ok {
				break Loop
			}
			read(raw)
		case <-p.producerDone:
			// The executor returned. Its sends all completed before it did,
			// but pick up anything still buffered, then finish.
			for {
				select {
				case raw, ok := <-ch:
					if !ok {
						break Loop
					}
					read(raw)
				default:
					break Loop
				}
			}
		}
	}

	for _, snapshot := range acc.flush() {
		p.push(snapshot)
	}

	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *progressPipe) push(snapshot map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.pending = append(p.pending, snapshot)
	p.cond.Signal()
}

// stop discards undelivered snapshots and prevents any further handler calls.
// It does not wait for an in-flight handler invocation, which may be blocked.
func (p *progressPipe) stop() {
	p.mu.Lock()
	p.stopped = true
	p.pending = nil
	p.cond.Broadcast()
	p.mu.Unlock()
}

// wait blocks until the delivery goroutine has drained everything, or ctx is
// done. It reports whether delivery finished.
func (p *progressPipe) wait(ctx context.Context) bool {
	select {
	case <-p.finished:
		return true
	case <-ctx.Done():
		p.stop()
		return false
	}
}

func (p *progressPipe) deliver() {
	defer close(p.finished)

	for {
		p.mu.Lock()
		for len(p.pending) == 0 && !p.closed && !p.stopped {
			p.cond.Wait()
		}
		if p.stopped || len(p.pending) == 0 {
			p.mu.Unlock()
			return
		}
		snapshot := p.pending[0]
		p.pending = p.pending[1:]
		p.mu.Unlock()

		// Check cancellation at claim time so that, once the caller's context
		// is done, no further callback starts even if updates were already
		// queued. On success the context is still live, so this never drops
		// the final progress event.
		if p.ctx.Err() != nil {
			return
		}

		p.handler(snapshot)
	}
}
