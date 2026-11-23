// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package sound

import (
	"io"
	"sync"
)

// Pipe creates a pair of Source and Sink such that writes to the sink
// are passed through to reads from the Source.
//
// The returned source, sink are safe for use in multiple goroutines.
//
// Close may be called at any time.  Once Close returns, all Send and
// Receive calls which were blocked are guaranteed to return in finite
// time: data already written but not yet read is delivered to readers
// first, and subsequent reads (or reads made when no data remains)
// return io.EOF.  Sends made after Close return io.EOF.
//
// Pipe is the dual of zikichombo.org/sound/ops.Copy.
func Pipe(v Form) (Source, Sink) {
	p := &pipe{Form: v}
	p.cond = sync.NewCond(&p.mu)
	return p, p
}

type pipe struct {
	Form
	mu   sync.Mutex
	cond *sync.Cond
	once sync.Once
	// q holds frames written but not yet read, in channel
	// de-interleaved layout over qFrames frames with Channels()
	// channels.  Each chunk in qChunks is itself de-interleaved over
	// its own frame count; the chunks concatenate, in order, into q.
	qChunks [][]float64
	// qOff is the offset, in frames, already consumed inside the
	// first chunk of qChunks.
	qOff   int
	qFrms  int
	closed bool
}

func (p *pipe) Send(d []float64) error {
	nC := p.Channels()
	if len(d)%nC != 0 {
		return ErrChannelAlignment
	}
	if len(d) == 0 {
		// Nothing to deliver; only a closed pipe changes the answer.
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return io.EOF
		}
		return nil
	}
	// Take a private copy: the caller is free to reuse d as soon as
	// Send returns, even while the data is still buffered for readers.
	cp := make([]float64, len(d))
	copy(cp, d)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.EOF
	}
	p.qChunks = append(p.qChunks, cp)
	p.qFrms += len(cp) / nC
	// Wake every blocked reader; a reader whose buffer cannot be fully
	// satisfied may go back to sleep, but at least one can make progress.
	p.cond.Broadcast()
	return nil
}

func (p *pipe) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		// Release every blocked Send and Receive.
		p.cond.Broadcast()
	})
	return nil
}

func (p *pipe) Receive(dst []float64) (int, error) {
	nC := p.Channels()
	if len(dst)%nC != 0 {
		return 0, ErrChannelAlignment
	}
	want := len(dst) / nC
	p.mu.Lock()
	defer p.mu.Unlock()
	// Block until either enough frames have been queued to satisfy the
	// whole request, or the pipe is closed and no more frames can ever
	// arrive.  Closing a pipe therefore never strands a blocked reader:
	// it drains whatever remains, then gets io.EOF on the next call.
	for p.qFrms < want && !p.closed {
		p.cond.Wait()
	}
	if p.qFrms == 0 {
		// Closed with nothing left to deliver.
		return 0, io.EOF
	}
	// Deliver at most the requested number of frames.  Residual data
	// stays queued and is returned by subsequent Receives before EOF.
	n := want
	if p.qFrms < n {
		n = p.qFrms
	}
	p.deliverLocked(dst, n, nC)
	// Per the Source contract, a short receive is followed by EOF on
	// the next call; that is produced naturally once the queue drains.
	return n, nil
}

// deliverLocked copies n frames from the head of the queue into dst in
// channel de-interleaved layout (dst has capacity for at least n frames),
// and advances the queue.  Caller must hold p.mu.
func (p *pipe) deliverLocked(dst []float64, n, nC int) {
	written := 0
	for written < n {
		chunk := p.qChunks[0]
		cFrms := len(chunk) / nC
		avail := cFrms - p.qOff
		take := avail
		if n-written < take {
			take = n - written
		}
		for c := 0; c < nC; c++ {
			srcStart := c*cFrms + p.qOff
			dstStart := c*n + written
			copy(dst[dstStart:dstStart+take], chunk[srcStart:srcStart+take])
		}
		written += take
		p.qOff += take
		p.qFrms -= take
		if p.qOff == cFrms {
			// Whole chunk consumed; drop it so its backing array can
			// be collected.
			p.qChunks[0] = nil
			p.qChunks = p.qChunks[1:]
			p.qOff = 0
		}
	}
}
