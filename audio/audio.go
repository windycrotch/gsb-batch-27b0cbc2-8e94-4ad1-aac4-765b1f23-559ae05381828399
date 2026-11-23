package audio

import (
	"errors"
	"flag"
	"github.com/gordonklaus/portaudio"
	"sync"
	"time"

	"github.com/sqweek/sqribe/log"

	. "github.com/sqweek/sqribe/core/types"
)

type audioOps interface {
	Open(params portaudio.StreamParameters) (*portaudio.Stream, error)
	Append(samples []int16) int
	Prepare()
	Started()
	Index() (idx FrameN, ok bool)
}

var useCallback = flag.Bool("cb", false, "use callback")

/* mu guards every piece of mutable stream state below: ops, stream, stopped,
** fr, prevfr, baseIndex, prevBase. The portaudio callback thread, the blocking
** write thread and the GUI feedback thread all touch these, so all entry points
** take the lock rather than relying on benign races. */
var mu sync.RWMutex
var ops audioOps
var stream *portaudio.Stream

var stopped bool = true
var fr, prevfr FrameRange
var baseIndex, prevBase FrameN

func HostApi() *portaudio.HostApiInfo {
	/* TODO allow user to override host api */
	for _, api := range PlatformHostApis() {
		hostApi, err := portaudio.HostApi(api)
		if err == nil {
			return hostApi
		}
		log.AU.Printf("%v: %v", api, err)
	}
	return nil
}

var (
	Channels   int
	SampleRate int // aka Frame rate
)

func Open() error {
	err := portaudio.Initialize()
	if err != nil {
		return err
	}

	host := HostApi()
	if host == nil {
		return errors.New("no host APIs available!")
	}
	dev := host.DefaultOutputDevice
	params := portaudio.LowLatencyParameters(nil, dev)
	l := params.Output.Latency
	/* pulseaudio (via ALSA) uses heaps of CPU at the default low latency (~8ms) */
	for params.Output.Latency < 30*time.Millisecond {
		if params.Output.Latency+l > dev.DefaultHighOutputLatency {
			params.Output.Latency = dev.DefaultHighOutputLatency
			break
		}
		params.Output.Latency += l
	}
	if *useCallback {
		ops = cbOps()
	} else {
		ops = blockOps(params.Output.Channels)
	}
	s, err := ops.Open(params)
	if err != nil {
		return err
	}
	mu.Lock()
	stream = s
	stopped = true
	mu.Unlock()
	Channels = params.Output.Channels
	SampleRate = int(params.SampleRate)

	impl := "blocking"
	if *useCallback {
		impl = "callback"
	}
	log.AU.Printf("%s stream %s:'%s' (%d channels @ %d Hz) w/ latency %v\n", impl, host.Name, dev.Name, Channels, SampleRate, params.Output.Latency)

	return nil
}

func Shutdown() {
	portaudio.Terminate()
}

/* Append feeds samples into the active stream. It blocks (inside the ops
** implementation) when the device buffer is full, which applies natural
** back-pressure to the feeder. The frame bookkeeping is lock-protected. */
func Append(wav []int16) int {
	n := ops.Append(wav)
	mu.Lock()
	fr.Max += FrameN(n / Channels)
	mu.Unlock()
	return n
}

func Play(f0 FrameN) error {
	mu.Lock()
	defer mu.Unlock()
	if stopped {
		baseIndex = 0
		ops.Prepare()
		if err := stream.Start(); err != nil {
			return err
		}
		ops.Started()
		stopped = false
	} else {
		prevfr, prevBase = fr, baseIndex
		baseIndex += (prevfr.Max - prevfr.Min)
	}
	fr = FrameRange{f0, f0}
	return nil
}

func Stop() {
	mu.Lock()
	stopped = true
	s := stream
	mu.Unlock()
	if s != nil {
		s.Abort()
	}
}

func IsPlaying() bool {
	mu.RLock()
	defer mu.RUnlock()
	return !stopped
}

/* markStopped is used by the portaudio callback path to record that the stream
** has been aborted without taking the write lock on the audio thread. */
func markStopped() {
	mu.Lock()
	stopped = true
	mu.Unlock()
}

func isStopped() bool {
	mu.RLock()
	defer mu.RUnlock()
	return stopped
}

func PlayingFrame() (FrameN, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if stopped {
		return 0, false
	}
	index, ok := ops.Index()
	if index < baseIndex {
		/* haven't looped around yet */
		return prevfr.Min + (index - prevBase), ok
	}
	return fr.Min + (index - baseIndex), ok
}

/* currentBase returns the frame the stream position is measured from, used by
** the playback engine to track loop wraps deterministically. */
func currentBase() FrameN {
	mu.RLock()
	defer mu.RUnlock()
	return baseIndex
}
