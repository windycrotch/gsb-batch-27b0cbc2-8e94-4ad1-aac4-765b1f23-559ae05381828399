// SPDX-FileCopyrightText: 2025 Tomi P. Hakala
// SPDX-License-Identifier: LGPL-2.1-or-later

package resampler

import (
	"fmt"
	"math"
	"sync"

	pipelinepkg "github.com/tphakala/go-audio-resampler/internal/pipeline"
)

// constantRateResampler implements fixed-ratio resampling.
// It uses a multi-stage pipeline approach similar to libsoxr,
// combining different algorithms for optimal performance.
type constantRateResampler struct {
	config   Config
	ratio    float64
	pipeline *Pipeline

	// Per-channel state
	channels []*channelResampler

	// mu serializes the public state-advancing entry points (Process,
	// ProcessInto, ProcessFloat32Into, ProcessMulti, Flush, FlushMulti and
	// Reset; ProcessFloat32 delegates to Process). A drain must never
	// interleave with processing: Flush works on the same stage delay lines
	// that Process writes, so overlapping the two corrupts both. Holding mu for
	// a whole ProcessMulti call keeps the per-channel parallelism (its
	// goroutines run inside the critical section) while making a concurrent
	// FlushMulti wait for the chunk to finish. Two concurrent drains therefore
	// serialize: exactly one advances the state and the others observe its
	// completed, empty result instead of replaying tails.
	mu sync.Mutex

	// Grow-only float64 scratch buffers reused by ProcessFloat32Into to convert
	// float32 input/output without allocating on every call. The engine's
	// growStableLen/appendStable helpers live in internal/engine and are not
	// importable here, so the grow-only sizing is done inline.
	f32in  []float64
	f32out []float64
}

// channelResampler holds per-channel state.
type channelResampler struct {
	stages  []Stage
	buffers []*RingBuffer

	// Pre-allocated scratch buffer reused by ReadInto to avoid allocations.
	readScratch []float64

	// Flush lifecycle. A flush is resumable rather than all-or-nothing: each
	// channel is drained at most once per stream, and progress is kept on the
	// channel so a stage error does not discard the channels already drained
	// or make a retry inject the padding a second time.
	//
	// flushDone records that this channel's tail has already been returned;
	// further FlushMulti calls return an empty slice for it. flushStage is the
	// next stage to drain, and flushPending holds input read from
	// buffers[flushStage] that the stage has not yet accepted, so a failed
	// Stage.Process can be retried with the same samples instead of reading the
	// ring buffer again (which would lose them) or skipping them.
	flushDone    bool
	flushStage   int
	flushPending []float64
}

// beginStream clears the flush lifecycle so a Process after a completed flush
// starts a new stream instead of being reported as already drained. It is not
// called for empty input, so a stray Process(nil) does not undo a finished
// drain.
//
// If an earlier drain stopped on a stage error, the samples it had already
// read out of the ring buffer are handed back to the stage's input buffer
// rather than dropped, so continuing the stream after a failed flush does not
// lose input. (The supported recovery after an error is to call Flush again,
// which resumes from flushPending directly.)
func (ch *channelResampler) beginStream() {
	if len(ch.flushPending) > 0 && ch.flushStage < len(ch.stages) {
		ch.buffers[ch.flushStage].Write(ch.flushPending)
	}
	ch.clearFlushState()
}

// clearFlushState discards any drain progress without restoring it. Reset uses
// it because Reset has already cleared the buffers: writing retained input back
// would leave a fresh-looking instance with phantom samples in its first
// buffer, so its next Process would not match a newly constructed one.
func (ch *channelResampler) clearFlushState() {
	ch.flushDone = false
	ch.flushStage = 0
	ch.flushPending = nil
}

// newConstantRateResampler creates a new constant-rate resampler.
func newConstantRateResampler(config *Config, ratio float64) (*constantRateResampler, error) {
	r := &constantRateResampler{
		config:   *config,
		ratio:    ratio,
		channels: make([]*channelResampler, config.Channels),
	}

	// Build the processing pipeline based on quality and ratio
	pipeline, err := buildPipeline(config, ratio)
	if err != nil {
		return nil, fmt.Errorf("failed to build pipeline: %w", err)
	}
	r.pipeline = pipeline

	// Initialize per-channel state
	for i := range config.Channels {
		ch := &channelResampler{
			stages:  make([]Stage, len(pipeline.stages)),
			buffers: make([]*RingBuffer, len(pipeline.stages)+1),
		}

		// Create stage instances for this channel
		for j, stageSpec := range pipeline.stages {
			stage, err := createStage(stageSpec, config)
			if err != nil {
				return nil, fmt.Errorf("failed to create stage %d: %w", j, err)
			}
			ch.stages[j] = stage
		}

		// Create buffers between stages
		for j := 0; j <= len(pipeline.stages); j++ {
			bufferSize := defaultBufferSize
			if config.MaxInputSize > 0 && j == 0 {
				bufferSize = config.MaxInputSize * bufferSizeMultiplier
			}
			ch.buffers[j] = NewRingBuffer(bufferSize)
		}

		r.channels[i] = ch
	}

	return r, nil
}

// Process resamples a mono audio channel.
//
// Process and a concurrent Flush on the same instance are serialized: the
// drain cannot run against the delay lines a Process is writing.
func (r *constantRateResampler) Process(input []float64) ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.channels) == 0 {
		return nil, fmt.Errorf("no channels initialized")
	}

	// Use first channel for mono processing
	return r.processChannel(0, input)
}

// ProcessInto resamples input into the caller-provided output buffer.
// It writes up to len(output) samples and returns the number of valid samples;
// callers should consume output[:n]. The buffer tail beyond n is undefined.
//
// If output is too small, ProcessInto returns ErrBufferTooSmall before any
// processing state is advanced, so callers can retry with a larger buffer.
func (r *constantRateResampler) ProcessInto(input, output []float64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.channels) == 0 {
		return 0, fmt.Errorf("no channels initialized")
	}
	if len(output) < r.EstimateOutput(len(input)) {
		return 0, ErrBufferTooSmall
	}

	return r.processChannelInto(0, input, output)
}

// EstimateOutput returns the maximum number of output samples that
// processing inputLen input samples may produce. Callers should allocate
// output buffers of at least this size for ProcessInto.
func (r *constantRateResampler) EstimateOutput(inputLen int) int {
	return int(float64(inputLen)*r.ratio) + estimateOutputMargin
}

// ProcessFloat32 resamples float32 audio data.
// Internally converts to float64 for processing, then converts back.
// This approach maintains numerical precision during resampling while
// supporting float32 I/O. The conversion overhead is minimal compared
// to the filter computation (~5% of total time for typical buffers).
// Native float32 processing would require duplicating the engine with
// generic types, which is a trade-off between code complexity and performance.
//
// It does not lock the instance itself: the conversion touches only local
// memory and the delegated Process call takes the lock.
func (r *constantRateResampler) ProcessFloat32(input []float32) ([]float32, error) {
	// Convert to float64 for high-precision internal processing
	input64 := make([]float64, len(input))
	for i, v := range input {
		input64[i] = float64(v)
	}

	output64, err := r.Process(input64)
	if err != nil {
		return nil, err
	}

	output32 := make([]float32, len(output64))
	for i, v := range output64 {
		output32[i] = float32(v)
	}

	return output32, nil
}

// ProcessFloat32Into resamples float32 input into the caller-provided float32
// output buffer. It is the caller-owned-output, float32 counterpart of
// ProcessInto: it writes up to len(output) samples, returns the count, and
// returns ErrBufferTooSmall before advancing state if output cannot hold
// EstimateOutput(len(input)) samples.
//
// Unlike ProcessFloat32, which allocates an input, intermediate, and output
// slice on every call, this method reuses grow-only internal scratch buffers
// for the float32<->float64 conversion, so it performs zero allocations once
// warm. Processing still runs through the float64 pipeline for precision.
//
// Single-channel only, matching ProcessFloat32 and the float64 ProcessInto.
// Like the other public entry points it is serialized against a concurrent
// drain on the same instance.
func (r *constantRateResampler) ProcessFloat32Into(input, output []float32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.channels) == 0 {
		return 0, fmt.Errorf("no channels initialized")
	}
	required := r.EstimateOutput(len(input))
	if len(output) < required {
		return 0, ErrBufferTooSmall // checked before any state is advanced
	}

	// Grow-only float32 -> float64 input scratch.
	if cap(r.f32in) < len(input) {
		r.f32in = make([]float64, len(input))
	} else {
		r.f32in = r.f32in[:len(input)]
	}
	for i, v := range input {
		r.f32in[i] = float64(v)
	}

	// Grow-only float64 output scratch sized to the estimated output bound, not
	// the caller's buffer. processChannelInto never produces more than
	// EstimateOutput samples, so sizing to required keeps the scratch bounded by
	// input length instead of letting an oversized caller buffer grow it without
	// limit for the lifetime of the resampler.
	if cap(r.f32out) < required {
		r.f32out = make([]float64, required)
	} else {
		r.f32out = r.f32out[:required]
	}

	n, err := r.processChannelInto(0, r.f32in, r.f32out)
	if err != nil {
		return 0, err
	}
	for i := range n {
		output[i] = float32(r.f32out[i])
	}
	return n, nil
}

// ProcessMulti processes multiple audio channels.
// When EnableParallel is true in config, channels are processed concurrently.
// Otherwise, channels are processed sequentially.
//
// The whole call runs under the instance lock, so no Flush can interleave with
// the per-channel stage updates. Parallel channels inside one call still run
// concurrently; each touches only its own channel state.
func (r *constantRateResampler) ProcessMulti(input [][]float64) ([][]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(input) != r.config.Channels {
		return nil, fmt.Errorf("expected %d channels, got %d", r.config.Channels, len(input))
	}

	output := make([][]float64, len(input))

	// Sequential processing (default or when parallel disabled)
	if !r.config.EnableParallel || len(input) <= 1 {
		for ch := range input {
			result, err := r.processChannel(ch, input[ch])
			if err != nil {
				return nil, fmt.Errorf("channel %d: %w", ch, err)
			}
			output[ch] = result
		}
		return output, nil
	}

	// Parallel processing: process channels concurrently
	var wg sync.WaitGroup
	errChan := make(chan error, len(input))

	for ch := range input {
		wg.Add(1)
		go func(channel int) {
			defer wg.Done()

			result, err := r.processChannel(channel, input[channel])
			if err != nil {
				errChan <- fmt.Errorf("channel %d: %w", channel, err)
				return
			}
			output[channel] = result
		}(ch)
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	return output, nil
}

// processChannel processes a single channel through the pipeline.
func (r *constantRateResampler) processChannel(channel int, input []float64) ([]float64, error) {
	if channel >= len(r.channels) {
		return nil, fmt.Errorf("channel %d out of range", channel)
	}

	ch := r.channels[channel]

	// Non-empty input starts (or continues) a stream. After a completed
	// drain, the stages are back at their fresh state, so this only re-arms
	// the flush lifecycle; it does not touch filter state.
	if len(input) > 0 {
		ch.beginStream()
	}

	// Add input to first buffer
	ch.buffers[0].Write(input)

	// Process through each stage
	for i, stage := range ch.stages {
		inputBuffer := ch.buffers[i]
		outputBuffer := ch.buffers[i+1]

		// Read all available input in a single pass (matching processChannelInto)
		// rather than looping over GetMinInput()-sized chunks. The stages are
		// streaming-stateful, so chunk granularity does not affect the output
		// (verified bit-identical by TestNewPath_ProcessInto_MatchesProcess), and
		// one large read trims per-iteration overhead on this allocating path.
		// Reading the whole buffer drains it in one go, so this is a single
		// guarded read rather than a loop.
		if avail := inputBuffer.Available(); avail >= stage.GetMinInput() {
			chunk := inputBuffer.Read(avail)

			// Process through stage
			output, err := stage.Process(chunk)
			if err != nil {
				return nil, fmt.Errorf("stage %d processing error: %w", i, err)
			}

			// Write to next buffer
			outputBuffer.Write(output)
		}
	}

	// Read final output
	finalBuffer := ch.buffers[len(ch.buffers)-1]
	return finalBuffer.ReadAll(), nil
}

// processChannelInto processes a single channel through the pipeline using the
// zero-copy path when available, and writes the output into dst. Returns the
// number of samples written.
func (r *constantRateResampler) processChannelInto(channel int, input, dst []float64) (int, error) {
	if channel >= len(r.channels) {
		return 0, fmt.Errorf("channel %d out of range", channel)
	}

	ch := r.channels[channel]

	if len(input) > 0 {
		ch.beginStream()
	}

	ch.buffers[0].Write(input)

	for i, stage := range ch.stages {
		inputBuffer := ch.buffers[i]
		outputBuffer := ch.buffers[i+1]

		zcStage, hasZC := stage.(pipelinepkg.ZeroCopyProcessor)

		for inputBuffer.Available() >= stage.GetMinInput() {
			avail := inputBuffer.Available()
			if cap(ch.readScratch) < avail {
				ch.readScratch = make([]float64, avail)
			} else {
				ch.readScratch = ch.readScratch[:avail]
			}
			n := inputBuffer.ReadInto(ch.readScratch)
			chunk := ch.readScratch[:n]

			var output []float64
			var err error
			if hasZC {
				output, err = zcStage.ProcessZeroCopy(chunk)
			} else {
				output, err = stage.Process(chunk)
			}
			if err != nil {
				return 0, fmt.Errorf("stage %d processing error: %w", i, err)
			}

			outputBuffer.Write(output)
		}
	}

	finalBuffer := ch.buffers[len(ch.buffers)-1]
	if finalBuffer.Available() > len(dst) {
		panic("go-audio-resampler: EstimateOutput underestimated actual output length")
	}
	n := finalBuffer.ReadInto(dst)
	return n, nil
}

// Flush drains channel 0's pipeline. For multi-channel streams processed
// via ProcessMulti, use FlushMulti to drain every channel.
//
// Flush is end-of-stream only and terminal for the drained stream: a second
// Flush call returns an empty slice without touching the filter state, and a
// later non-empty Process starts a new stream. The drain is resumable: if a
// stage fails partway, the samples already drained for channel 0 are kept (in
// the internal buffers) rather than dropped, and calling Flush again continues
// where it stopped instead of re-injecting the flush padding.
func (r *constantRateResampler) Flush() ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.channels) == 0 {
		return []float64{}, nil
	}
	return r.flushChannel(0, r.channels[0])
}

// flushChannel drains one channel's multi-stage pipeline, resuming from where
// a previous attempt stopped.
//
// Stages are flushed front-to-back: pending input (including the previous
// stage's flushed tail) is processed through the stage before flushing its
// delay line, so the tail propagates all the way to the final buffer
// (issue #37).
//
// The channel's progress lives in channelResampler (flushStage, flushPending,
// flushDone) rather than in local variables, so a stage error can return
// without losing the work already done. On error the caller gets the channels
// that finished (FlushMulti) and a retry on the same instance only completes
// the ones that did not, without replaying any padding. The final buffer keeps
// the samples produced before the error, so nothing drained is discarded.
func (r *constantRateResampler) flushChannel(chIdx int, ch *channelResampler) ([]float64, error) {
	if ch.flushDone {
		return []float64{}, nil
	}

	for i := ch.flushStage; i < len(ch.stages); i++ {
		stage := ch.stages[i]
		inputBuffer := ch.buffers[i]
		outputBuffer := ch.buffers[i+1]

		// Anything the stage could not accept yet, including input read by a
		// previous failed attempt, is held in flushPending so a retry feeds
		// the same samples again. Reading the ring buffer is destructive, so
		// it is read at most once.
		if len(ch.flushPending) == 0 {
			if avail := inputBuffer.Available(); avail > 0 {
				ch.flushPending = inputBuffer.Read(avail)
			}
		}
		if len(ch.flushPending) > 0 {
			out, err := stage.Process(ch.flushPending)
			if err != nil {
				ch.flushStage = i
				return nil, fmt.Errorf("channel %d stage %d flush-process error: %w", chIdx, i, err)
			}
			if len(out) > 0 {
				outputBuffer.Write(out)
			}
			ch.flushPending = nil
		}

		out, err := stage.Flush()
		if err != nil {
			ch.flushStage = i
			return nil, fmt.Errorf("channel %d stage %d flush error: %w", chIdx, i, err)
		}
		if len(out) > 0 {
			outputBuffer.Write(out)
		}
		ch.flushStage = i + 1
	}

	finalBuffer := ch.buffers[len(ch.buffers)-1]
	ch.flushDone = true
	ch.flushStage = 0
	return finalBuffer.ReadAll(), nil
}

// FlushMulti flushes every channel's pipeline independently, returning one
// slice per channel.
//
// Each channel is drained exactly once per stream: the returned slice for a
// channel whose tail was already returned (by an earlier FlushMulti or a
// Flush) is empty. On a stage error the return value keeps the per-channel
// data of the channels that did finish, plus empty slices for the rest, and
// the error. Calling FlushMulti again on the same instance resumes the
// unfinished channels only, so concatenating a channel's results across the
// attempts yields exactly what one successful FlushMulti would have returned.
// Concurrent FlushMulti calls on one instance serialize; all but the one that
// advances the state see the completed, empty result.
func (r *constantRateResampler) FlushMulti() ([][]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.channels) == 0 {
		return [][]float64{}, nil
	}

	output := make([][]float64, len(r.channels))
	var firstErr error
	for chIdx, ch := range r.channels {
		// Keep draining the other channels after a failure: their tails are
		// independent, so returning them now preserves work a retry would
		// otherwise have to (and could not) reproduce as partial results.
		flushed, err := r.flushChannel(chIdx, ch)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// flushChannel returns nil on error; report an empty slice so the
			// caller can index the result by channel.
			if flushed == nil {
				flushed = []float64{}
			}
		}
		output[chIdx] = flushed
	}
	return output, firstErr
}

// startupDeficitStage is the accounting contract stages provide for accurate
// latency reporting: the un-rounded startup deficit in the stage's own
// output-sample domain. Compile-time assertions in stages.go keep every
// production stage type on this path.
type startupDeficitStage interface {
	StartupDeficit() float64
}

// GetLatency returns the pipeline's startup deficit in output samples: how
// many samples early Process calls withhold while the stage filters prime.
// Each stage's deficit is converted into the final output rate domain
// through the downstream stages' ratios before summing.
func (r *constantRateResampler) GetLatency() int {
	if r.pipeline == nil || len(r.channels) == 0 {
		return 0
	}
	ch := r.channels[0]
	if ch == nil || len(ch.stages) == 0 {
		return 0
	}
	total := 0.0
	for i, stage := range ch.stages {
		var deficit float64
		if s, ok := stage.(startupDeficitStage); ok {
			deficit = s.StartupDeficit()
		} else {
			// Fallback for stages without deficit accounting: group-delay
			// heuristic converted to the stage's output domain.
			deficit = float64(stage.GetLatency()) * stage.GetRatio()
		}
		for _, downstream := range ch.stages[i+1:] {
			deficit *= downstream.GetRatio()
		}
		total += deficit
	}
	return int(math.Ceil(total))
}

// Reset clears all internal state.
//
// After Reset the instance behaves like a freshly constructed one: any
// partially or fully completed flush lifecycle is discarded, so the instance
// can be used for a brand-new stream and drains exactly once at its end.
// Reset is serialized against Process and Flush on the same instance.
func (r *constantRateResampler) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The lock serializes Reset against the other public entry points (see mu).
	for _, ch := range r.channels {
		// Reset all stages
		for _, stage := range ch.stages {
			stage.Reset()
		}

		// Clear all buffers
		for _, buffer := range ch.buffers {
			buffer.Clear()
		}

		ch.clearFlushState()
	}
}

// GetRatio returns the resampling ratio.
func (r *constantRateResampler) GetRatio() float64 {
	return r.ratio
}

// GetInfo returns information about the resampler.
func (r *constantRateResampler) GetInfo() Info {
	info := Info{
		Algorithm: "multi-stage",
		Latency:   r.GetLatency(),
	}

	// Calculate total memory usage
	var memUsage int64
	for _, ch := range r.channels {
		for _, buffer := range ch.buffers {
			memUsage += int64(buffer.Capacity() * bytesPerFloat64)
		}
		for _, stage := range ch.stages {
			memUsage += stage.GetMemoryUsage()
		}
	}
	info.MemoryUsage = memUsage

	// Get stage information from first channel
	if len(r.channels) > 0 && len(r.channels[0].stages) > 0 {
		// Report info from primary stage
		primaryStage := r.channels[0].stages[0]
		info.FilterLength = primaryStage.GetFilterLength()
		info.Phases = primaryStage.GetPhases()

		// Check for SIMD
		if simd := primaryStage.GetSIMDInfo(); simd != "" {
			info.SIMDEnabled = true
			info.SIMDType = simd
		}
	}

	return info
}
