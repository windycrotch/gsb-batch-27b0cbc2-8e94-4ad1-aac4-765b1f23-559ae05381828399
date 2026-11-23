package wave

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/sqweek/sqribe/core/types"
	"github.com/sqweek/sqribe/log"
)

type IOError error
type DecodeError error

var (
	failure = &Chunk{} // non-nil sentinel used to avoid deadlock on i/o failure
	// ErrCacheUnavailable indicates that cache data could not be read: either a
	// chunk failed to load, the requested data is past the end of the stream, or
	// the wait was aborted by context cancellation.
	ErrCacheUnavailable = contextError("wave cache: data unavailable")
	// ErrCacheEOF indicates that a requested chunk lies beyond the end of the
	// fully-decoded stream.
	ErrCacheEOF = contextError("wave cache: read past end of stream")
)

type contextError string

func (e contextError) Error() string { return string(e) }

type cache struct {
	blocksz uint         /* block size in bytes */
	sampsz  uint         /* number of bytes to store one sample */
	mu      sync.RWMutex /* guards chunks, bytesWritten, lastChunkId, lastChunkSize, listeners */
	chunks  map[uint64]*Chunk
	lru     ChunkList

	file   string      /* backing filename */
	iochan chan uint64 /* list of blocks that need fetching */

	iodone    *sync.Cond  /* triggered whenever a block completes */
	closed    atomic.Bool /* set by Close so requeue timers never send on a closed channel */
	listeners []chan *Chunk

	bytesWritten  int64  /* -1 if decoding & writing has finished; access via progress() */
	lastChunkId   uint64 /* id of the last valid chunk */
	lastChunkSize uint   /* number of bytes in last chunk */
}

func mkcache(blocksz, sampsz uint, file string) *cache {
	cache := cache{blocksz: blocksz, sampsz: sampsz, file: file}
	cache.iochan = make(chan uint64, 20)
	cache.chunks = make(map[uint64]*Chunk)
	cache.listeners = make([]chan *Chunk, 0, 10)
	cache.iodone = sync.NewCond(&sync.Mutex{})
	cache.lru.max = 100
	cache.lru.cache = &cache
	go cache.fetcher()
	return &cache
}

func (c *cache) Close() {
	c.closed.Store(true)
	close(c.iochan)
}

func (c *cache) MaxSize() uint64 {
	return uint64(c.blocksz) * uint64(c.lru.max)
}

// progress returns the number of bytes decoded so far, or -1 once decoding has
// finished. Safe for concurrent access.
func (c *cache) progress() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bytesWritten
}

// finished reports whether decoding & writing is complete.
func (c *cache) finished() bool {
	return c.progress() == -1
}

// Writes out the cache file, given a func which successively spits out decoded
// wave samples, and returns io.EOF when complete. If the returned error is
// non-nil, it will be of type IOError or DecodeError depending on where it occurred.
func (c *cache) Write(readfn func() ([]int16, error)) error {
	defer func() {
		c.mu.Lock()
		c.lastChunkId = uint64(c.bytesWritten / int64(c.blocksz))
		c.lastChunkSize = uint(c.bytesWritten % int64(c.blocksz))
		c.bytesWritten = -1
		log.WAV.Printf("cache written: last=%d %d\n", c.lastChunkId, c.lastChunkSize)
		listeners := append([]chan *Chunk(nil), c.listeners...)
		c.mu.Unlock()
		c.iodone.L.Lock()
		c.iodone.Broadcast()
		c.iodone.L.Unlock()
		for _, l := range listeners {
			l := l
			go func() { l <- nil }()
		}
	}()
	f, err := os.Create(c.file)
	if err != nil {
		return IOError(err)
	}
	defer f.Close()
	for {
		buf, err := readfn()
		if err != nil && err != io.EOF {
			return DecodeError(err)
		}
		if err == io.EOF {
			break
		}
		if len(buf) > 0 {
			err = binary.Write(f, binary.LittleEndian, buf)
			if err != nil {
				return IOError(err)
			}
			c.mu.Lock()
			c.bytesWritten += int64(len(buf)) * int64(c.sampsz)
			c.mu.Unlock()
		}
	}
	return nil
}

func (c *cache) Bounds(sample0, sampleN SampleN) (uint64, uint64) {
	return c.Containing(sample0), c.Containing(sampleN)
}

func (c *cache) Containing(sample SampleN) uint64 {
	return uint64((sample * SampleN(c.sampsz)) / SampleN(c.blocksz))
}

func (c *cache) Get(id uint64) *Chunk {
	if c.finished() && id > c.lastChunkIdLocked() {
		return nil
	}
	c.iochan <- id /* signal to fetcher goroutine that this chunk is active */
	c.mu.RLock()
	chunk, ok := c.chunks[id] /* grab chunk if its already in cache */
	c.mu.RUnlock()
	if !ok || chunk == failure {
		return nil
	}
	return chunk
}

func (c *cache) lastChunkIdLocked() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastChunkId
}

/* wait polls for the chunk until it is available. It returns ErrCacheEOF for
** chunks past the end of a fully decoded stream, ErrCacheUnavailable on i/o
** failure, and ctx.Err() if the context is cancelled while waiting. It never
** blocks beyond the context deadline. */
func (c *cache) wait(ctx context.Context, id uint64) (*Chunk, error) {
	if chunk, err := c.peek(id); chunk != nil || err != nil {
		return chunk, err
	}
	// ask the fetcher to load this chunk; non-blocking so a full request queue
	// (it is re-requested on the next poll anyway) cannot stall the caller.
	select {
	case c.iochan <- id:
	default:
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if chunk, err := c.peek(id); chunk != nil || err != nil {
				return chunk, err
			}
			select {
			case c.iochan <- id:
			default:
			}
		}
	}
}

/* peek performs one non-blocking check for a chunk, taking the iodone lock in
** the same way the old Wait() did so it cannot miss a completion broadcast. */
func (c *cache) peek(id uint64) (*Chunk, error) {
	c.iodone.L.Lock()
	c.mu.RLock()
	chunk, ok := c.chunks[id]
	finished := c.bytesWritten == -1
	last := c.lastChunkId
	c.mu.RUnlock()
	c.iodone.L.Unlock()
	switch {
	case ok && chunk != failure:
		return chunk, nil
	case chunk == failure:
		return nil, ErrCacheUnavailable
	case finished && id > last:
		return nil, ErrCacheEOF
	}
	return nil, nil
}

/* blocks waiting for the chunk to be read; retained for callers without a
** context. Prefer wait(). */
func (c *cache) Wait(id uint64) *Chunk {
	chunk, _ := c.wait(context.Background(), id)
	return chunk
}

/* requeueLater returns an unwritten block to the fetch queue after a short
** delay. It checks the closed flag after sleeping so it can never panic by
** sending on the closed iochan during shutdown. */
func (c *cache) requeueLater(id uint64) {
	time.Sleep(500 * time.Millisecond)
	if c.closed.Load() {
		return
	}
	defer func() {
		// Close may still race the flag check; recover the benign close-send.
		_ = recover()
	}()
	select {
	case c.iochan <- id:
	default:
	}
}

func (c *cache) fetcher() {
	var fd *os.File
	for id := range c.iochan {
		c.mu.RLock()
		chunk, ok := c.chunks[id]
		c.mu.RUnlock()
		if ok && chunk != failure {
			/* chunk already in cache, no i/o necessary just bump the lru */
			c.lru.touch(chunk)
			continue
		}
		filename, offset := c.pos(id)
		if offset == -1 {
			/* block not written yet - back on the queue. A detached timer would
			** risk sending on a closed channel after Close, so requeue inline
			** through a helper that observes the closed flag. */
			log.WAV.Printf("fetcher: requeing block %d\n", id)
			go c.requeueLater(id)
			continue
		}
		if c.finished() && id > c.lastChunkIdLocked() {
			/* this chunk is past EOF; cache nil */
			c.add(id, nil)
			continue
		}
		if fd == nil {
			var err error
			if fd, err = os.Open(filename); err != nil {
				log.WAV.Printf("fetcher: %v\n", err)
				continue
			}
			defer fd.Close()
		}
		chunk, err := c.readchunk(id, fd, offset)
		if err != nil {
			log.WAV.Printf("fetcher: chunk %d: %v\n", id, err)
			c.add(id, failure)
			continue
		}
		log.WAV.Printf("fetcher: read chunk %d: i0=%d len=%d\n", id, chunk.I0, len(chunk.Data))
		c.add(id, chunk)
	}
}

func (c *cache) pos(id uint64) (string, int64) {
	offset := int64(id) * int64(c.blocksz)
	if !c.finished() && offset+int64(c.blocksz) > c.progress() {
		offset = -1 /* cache still initialising, block not written yet */
	}
	return c.file, offset
}

func (c *cache) add(id uint64, chunk *Chunk) {
	c.mu.Lock()
	c.chunks[id] = chunk
	listeners := append([]chan *Chunk(nil), c.listeners...)
	c.mu.Unlock()
	c.iodone.L.Lock()
	c.iodone.Broadcast()
	c.iodone.L.Unlock()
	if chunk == nil || chunk == failure {
		return // don't add nil or failure to the LRU
	}
	for _, l := range listeners {
		l := l
		go func() { l <- chunk }()
	}
	c.lru.addMu(chunk)
}

func (c *cache) listen() <-chan *Chunk {
	listener := make(chan *Chunk)
	c.mu.Lock()
	c.listeners = append(c.listeners, listener)
	c.mu.Unlock()
	return listener
}

func (c *cache) ignore(listener <-chan *Chunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, l := range c.listeners {
		if l == listener {
			close(c.listeners[i])
			c.listeners = append(c.listeners[:i], c.listeners[i+1:]...)
			return
		}
	}
}

type ChunkNode struct {
	chunk *Chunk
	next  *ChunkNode
	prev  *ChunkNode
}

type ChunkList struct {
	cache *cache
	size  uint
	max   uint
	head  *ChunkNode
	tail  *ChunkNode
}

func (l *ChunkList) add(chunk *Chunk) *Chunk {
	return l.addMu(chunk)
}

/* addMu inserts chunk and evicts the LRU entry when full, removing the evicted
** entry from the owning cache's map under the cache lock. */
func (l *ChunkList) addMu(chunk *Chunk) *Chunk {
	if l.cache != nil {
		l.cache.mu.Lock()
		defer l.cache.mu.Unlock()
	}
	node := &ChunkNode{chunk: chunk, next: l.head}
	if l.head == nil {
		l.tail = node
	} else {
		l.head.prev = node
	}
	l.head = node
	if l.size >= l.max {
		gone := l.tail
		l.tail = gone.prev
		if l.tail != nil {
			l.tail.next = nil
		}
		if l.cache != nil && gone.chunk != nil {
			delete(l.cache.chunks, gone.chunk.id)
		}
		return gone.chunk
	}
	l.size++
	return nil
}

func (l *ChunkList) promote(node *ChunkNode) {
	if node == l.head {
		return
	}
	node.prev.next = node.next
	if node.next != nil {
		node.next.prev = node.prev
	} else { /* node must be tail */
		l.tail = node.prev
	}
	node.prev = nil
	node.next = l.head
	l.head.prev = node
	l.head = node
}

func (l *ChunkList) touch(chunk *Chunk) {
	if chunk == nil {
		return
	}
	/* having to walk the list is daft... */
	for node := l.head; node != nil; node = node.next {
		if node.chunk == chunk {
			l.promote(node)
			return
		}
	}
}

func (c *cache) readchunk(id uint64, file *os.File, offset int64) (*Chunk, error) {
	_, err := file.Seek(offset, 0)
	if err != nil {
		return nil, err
	}
	nbytes := c.blocksz
	if c.finished() && id == c.lastChunkIdLocked() {
		c.mu.RLock()
		nbytes = c.lastChunkSize
		c.mu.RUnlock()
	}
	chunk := Chunk{I0: SampleN(offset) / SampleN(c.sampsz), Data: make([]int16, nbytes/c.sampsz), id: id}
	err = binary.Read(file, binary.LittleEndian, chunk.Data)
	return &chunk, err
}

type Chunk struct {
	I0   SampleN //first sample's index
	Data []int16

	id uint64 //index into cache.chunks
}

func (chunk *Chunk) Intersects(s0, sN SampleN) bool {
	cN := chunk.I0 + SampleN(len(chunk.Data)) - 1
	return (s0 >= chunk.I0 && s0 <= cN) || (sN >= chunk.I0 && sN <= cN)
}
