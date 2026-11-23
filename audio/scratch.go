package audio

import (
	"sync"

	"github.com/mowshon/moviego/v2/clip"
)

// scratchPool recycles the per-pull AudioBuffers a node uses to decode child
// runs. Pooling — instead of one buffer pinned to the node — is what makes a
// node safe for concurrent renders: every in-flight SamplesInto checks out its
// own buffer and returns it on every path (including a canceled pull, via
// defer), so two renders pulling the same node never observe each other's
// partial child data.
type scratchPool struct {
	mu   sync.Mutex
	free []*clip.AudioBuffer
}

func newScratchPool() *scratchPool { return &scratchPool{} }

// get returns a buffer, reusing a previously returned one when available.
func (p *scratchPool) get() *clip.AudioBuffer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.free); n > 0 {
		b := p.free[n-1]
		p.free[n-1] = nil
		p.free = p.free[:n-1]
		return b
	}
	return &clip.AudioBuffer{}
}

// put returns a buffer to the pool for reuse.
func (p *scratchPool) put(b *clip.AudioBuffer) {
	p.mu.Lock()
	p.free = append(p.free, b)
	p.mu.Unlock()
}
