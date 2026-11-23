package wave

import (
	"context"
	"github.com/sqweek/ffau"
	"sync/atomic"
	"time"

	"github.com/sqweek/sqribe/audio"
	. "github.com/sqweek/sqribe/core/types"
	"github.com/sqweek/sqribe/log"
)

type Waveform struct {
	nSamples atomic.Uint64
	Channels int
	rate     int
	Max      []int16 // maximum amplitudes for each channel (written by the decoder goroutine only)

	cache *cache
}

// NSamples returns the number of samples decoded so far. Safe for concurrent
// access while the decoder goroutine is still writing.
func (wav *Waveform) NSamplesCount() SampleN {
	return SampleN(wav.nSamples.Load())
}

func (wav *Waveform) addSamples(n SampleN) {
	wav.nSamples.Add(uint64(n))
}

func NewWaveform(file, cachefile string, reply chan<- error) (*Waveform, error) {
	wave := &Waveform{rate: audio.SampleRate, Channels: audio.Channels}
	wave.cache = mkcache(1024*1024, 2, cachefile)
	wave.Max = make([]int16, wave.Channels)
	ctx, err := ffau.OpenFile(file)
	if err != nil {
		return nil, err
	}
	raw, err := ctx.OpenAudioStream()
	if err != nil {
		return nil, err
	}
	log.WAV.Println("raw audiostream format", raw.Format())
	desired := ffau.AudioFormat{wave.rate, ffau.PackedS16s, ffau.DefaultLayout(wave.Channels)}
	converted, err := ffau.Resample(raw, desired)
	if err != nil {
		return nil, err
	}
	log.WAV.Println("converted audiostream format", converted.Format())
	reader, err := ffau.NewPackedS16Stream(converted)
	if err != nil {
		return nil, err
	}
	go func() {
		decode := func() ([]int16, error) {
			samps, err := reader.Read()
			if err != nil {
				return samps, err
			}
			if len(samps) > 0 {
				base := wave.NSamplesCount()
				for i := 0; i < len(samps); i++ {
					c := int((base + SampleN(i)) % SampleN(wave.Channels))
					if samps[i] > wave.Max[c] {
						wave.Max[c] = samps[i]
					} else if -samps[i] > wave.Max[c] {
						wave.Max[c] = -samps[i]
					}
				}
				wave.addSamples(SampleN(len(samps)))
			}
			return samps, nil
		}
		err := wave.cache.Write(decode)
		reply <- err
		if err != nil {
			log.WAV.Printf("decoding error %d samples into %s: %v", wave.NSamplesCount(), file, err)
		}
		converted.Close()
		ctx.Close()
	}()

	return wave, nil
}

func (wav *Waveform) Close() {
	// wait for decoder to finish, if it hasn't already
	for !wav.cache.finished() {
		time.Sleep(100 * time.Millisecond)
	}
	wav.cache.Close()
}

func (wav *Waveform) ChannelExtents(samples []int16) []int16 {
	minMax := make([]int16, wav.Channels*2)
	for i := 0; i < len(samples); i += wav.Channels {
		for j := 0; j < wav.Channels; j++ {
			if samples[i+j] < minMax[j*2] {
				minMax[j*2] = samples[i+j]
			} else if samples[i+j] > minMax[j*2+1] {
				minMax[j*2+1] = samples[i+j]
			}
		}
	}
	return minMax
}

func (chunk *Chunk) copy(samples []int16, i0 SampleN) {
	var c0, cN, s0, sN SampleN
	cN, sN = SampleN(len(chunk.Data)), SampleN(len(samples))
	if chunk.I0 > i0 {
		s0 = chunk.I0 - i0
	} else {
		c0 = i0 - chunk.I0
	}
	nc, ns := cN-c0, sN-s0
	if nc < 0 || ns < 0 {
		return
	}
	if nc < ns {
		sN = s0 + nc
	} else if nc > ns {
		cN = c0 + ns
	}
	copy(samples[s0:sN], chunk.Data[c0:cN])
}

/* FramesCtx blocks until frames from f0 to fN (inclusive) have been read from
** disk, or until ctx is cancelled / the cache reports the data can never be
** available. The returned error is non-nil on failure, in which case callers
** must not use the sample slice. */
func (wav *Waveform) FramesCtx(ctx context.Context, f0, fN FrameN) ([]int16, error) {
	if fN < f0 {
		fN = f0
	}
	s0, sN := wav.SampleRange(f0, fN)
	chunk0, chunkN := wav.cache.Bounds(s0, sN)
	if chunk0 == chunkN {
		/* not across a chunk boundary, no need to copy */
		chunk, err := wav.cache.wait(ctx, chunk0)
		if err != nil {
			return nil, err
		}
		return chunk.Data[s0-chunk.I0 : sN-chunk.I0+1], nil
	}
	samples := make([]int16, sN-s0+1)
	chunks := make(chan chunkResult, chunkN-chunk0+1)
	for id := chunk0; id <= chunkN; id++ {
		go func(i uint64) {
			ch, err := wav.cache.wait(ctx, i)
			chunks <- chunkResult{ch, err}
		}(id)
	}
	for i := uint64(0); i <= chunkN-chunk0; i++ {
		res := <-chunks
		if res.err != nil {
			return nil, res.err
		}
		res.chunk.copy(samples, s0)
	}
	return samples, nil
}

type chunkResult struct {
	chunk *Chunk
	err   error
}

/* Frames blocks until frames from f0 to fN (inclusive) have been read from
** disk. Prefer FramesCtx in playback paths so waits remain interruptible. */
func (wav *Waveform) Frames(f0, fN FrameN) []int16 {
	samples, err := wav.FramesCtx(context.Background(), f0, fN)
	if err != nil {
		return nil
	}
	return samples
}

func Extract(chunks []*Chunk, s0, sN SampleN) []int16 {
	var samples []int16 = nil
	for _, chunk := range chunks {
		cN := chunk.I0 + SampleN(len(chunk.Data)) - 1
		if !chunk.Intersects(s0, sN) {
			continue
		}
		if s0 >= chunk.I0 && sN <= cN {
			/* samples are contained entirely within this chunk */
			i0 := s0 - chunk.I0
			return chunk.Data[i0 : i0+(sN-s0+1)]
		}
		if samples == nil {
			samples = make([]int16, sN-s0+1)
		}
		chunk.copy(samples, s0)
	}
	return samples
}

/* Gets all cached samples for frames in range f0 to fN (inclusive) */
func (wav *Waveform) GetFrames(f0, fN FrameN) []*Chunk {
	s0, sN := wav.SampleRange(f0, fN)
	chunk0, chunkN := wav.cache.Bounds(s0, sN)
	chunks := make([]*Chunk, 0, chunkN-chunk0+1)
	for chunkI := chunk0; chunkI <= chunkN; chunkI++ {
		chunk := wav.cache.Get(chunkI)
		if chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func (wav *Waveform) MaxAmp() int16 {
	m := wav.Max[0]
	for j := 1; j < len(wav.Max); j++ {
		if wav.Max[j] > m {
			m = wav.Max[j]
		}
	}
	return m
}

func (wav *Waveform) TimeAtFrame(frame FrameN) time.Duration {
	secs := float64(frame) / float64(wav.rate)
	return time.Duration(secs*1000000) * time.Microsecond
}

func (wav *Waveform) FrameAtTime(t time.Duration) FrameN {
	return FrameN(float64(t) / float64(time.Second) * float64(wav.rate))
}

func (wav *Waveform) ClipFrame(f FrameN) FrameN {
	if f < 0 {
		f = 0
	}
	last := wav.ToFrame(wav.NSamplesCount())
	if last > 0 && f >= last {
		f = last - 1
	}
	return f
}

func (wav *Waveform) ToFrame(sample SampleN) FrameN {
	return FrameN(sample / SampleN(wav.Channels))
}

func (wav *Waveform) ToSample(frame FrameN) SampleN {
	return SampleN(frame * FrameN(wav.Channels))
}

func (wav *Waveform) SampleRange(f0, fN FrameN) (s0, sN SampleN) {
	s0 = SampleN(f0 * FrameN(wav.Channels))
	sN = SampleN((fN+1)*FrameN(wav.Channels) - 1)
	return s0, sN
}

/* Clips f to the range [0, x] where x is unlimited if the cache is initialising, and
 * otherwise the last frame of the waveform subtracted by inset. */
func (wav *Waveform) Clip(f FrameN, inset FrameN) FrameN {
	if f < 0 {
		return 0
	}
	if wav.cache.finished() {
		max := wav.ToFrame(wav.NSamplesCount())
		if f+inset > max {
			return max - inset
		}
	}
	return f
}

/* CacheListen returns a channel on which a sample Chunk will be sent each time
 * one is read from disk. As a special case, nil is transmitted on the channel once
 * the cache is finished initialising (which signals that wav.NSamples is now stable). */
func (wav *Waveform) CacheListen() <-chan *Chunk {
	return wav.cache.listen()
}

func (wav *Waveform) CacheIgnore(listener <-chan *Chunk) {
	wav.cache.ignore(listener)
}

func (wav *Waveform) CacheSize() uint64 {
	return wav.cache.MaxSize()
}

type wavRange struct {
	wav *Waveform
}

func (rng wavRange) MinFrame() FrameN {
	return 0
}

func (rng wavRange) MaxFrame() FrameN {
	return rng.wav.ToFrame(rng.wav.NSamplesCount()) - 1
}

func Range(wav *Waveform) TimeRange {
	if wav == nil {
		return FrameRange{0, 0}
	}
	return &wavRange{wav}
}
