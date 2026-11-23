package audio

import (
	"github.com/gordonklaus/portaudio"
	"sync"

	. "github.com/sqweek/sqribe/core/types"
	"github.com/sqweek/sqribe/log"
)

type blockingOps struct {
	mu         sync.Mutex
	buf        []int16
	pos        int
	writes     int
	frameDelay FrameN
}

func blockOps(channels int) *blockingOps {
	return &blockingOps{buf: make([]int16, 1024*channels)}
}

func (block *blockingOps) Open(params portaudio.StreamParameters) (s *portaudio.Stream, err error) {
	s, err = portaudio.OpenStream(params, block.buf)
	if err == nil {
		info := s.Info()
		block.frameDelay = FrameN(info.OutputLatency.Seconds() * float64(info.SampleRate))
		log.AU.Println("frameDelay", block.frameDelay)
	}
	return
}

func (block *blockingOps) Append(wav []int16) int {
	block.mu.Lock()
	defer block.mu.Unlock()
	src := wav
	for len(src) > 0 {
		n := copy(block.buf[block.pos:], src)
		src = src[n:]
		block.pos += n
		if block.pos == len(block.buf) {
			stream.Write()
			block.pos = 0
			block.writes++
		}
	}
	return len(wav)
}

func (block *blockingOps) Prepare() {
	block.mu.Lock()
	block.pos = 0
	block.writes = 0
	block.mu.Unlock()
}

func (block *blockingOps) Started() {
}

func (block *blockingOps) Index() (FrameN, bool) {
	block.mu.Lock()
	written := FrameN((block.writes*len(block.buf) + block.pos) / Channels)
	block.mu.Unlock()
	if written < block.frameDelay {
		return 0, true
	}
	return written - block.frameDelay, true
}
