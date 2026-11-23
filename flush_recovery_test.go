// SPDX-FileCopyrightText: 2026 Tomi P. Hakala
// SPDX-License-Identifier: LGPL-2.1-or-later

package resampler

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// flakyStage wraps a real Stage and injects failures into draining. It lets a
// test drive the resumable FlushMulti lifecycle from the outside: a failing
// attempt must not lose the channels drained before it, and a retry must
// complete only the channels that did not finish.
//
// A failing call does not touch the wrapped stage, which models the stages'
// actual contract: a stage reports an error before it advances its delay line.
type flakyStage struct {
	Stage
	failFlush   int
	failProcess int
}

var errInjected = errors.New("injected stage failure")

func (s *flakyStage) Flush() ([]float64, error) {
	if s.failFlush > 0 {
		s.failFlush--
		return nil, errInjected
	}
	return s.Stage.Flush()
}

func (s *flakyStage) Process(input []float64) ([]float64, error) {
	if s.failProcess > 0 {
		s.failProcess--
		return nil, errInjected
	}
	return s.Stage.Process(input)
}

// drainReference returns the one-shot Process and FlushMulti results for a
// multi-channel stream on a fresh instance.
func drainReference(t *testing.T, channels int, inputs [][]float64) (proc, tails [][]float64) {
	t.Helper()

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
	require.NoError(t, err)

	proc, err = r.ProcessMulti(cloneChannels(inputs))
	require.NoError(t, err)

	mf, ok := r.(MultiFlusher)
	require.True(t, ok)

	tails, err = mf.FlushMulti()
	require.NoError(t, err)
	return proc, tails
}

func cloneChannels(input [][]float64) [][]float64 {
	out := make([][]float64, len(input))
	for i := range input {
		out[i] = slices.Clone(input[i])
	}
	return out
}

// testChannels builds per-channel input whose every sample differs between
// channels, so a duplicated or swapped tail is visible, not merely a length
// mismatch.
func testChannels(channels, n int) [][]float64 {
	in := make([][]float64, channels)
	for ch := range channels {
		in[ch] = make([]float64, n)
		for i := range in[ch] {
			in[ch][i] = 0.5*math.Sin(float64(i)*0.01) + 0.1*float64(ch)
		}
	}
	return in
}

// TestFlushMulti_FailedDrainResumesOnlyRemainingChannels pins the resumable
// lifecycle at both failure points: a stage error must not discard the channels
// already drained, and a retry on the same instance must complete only the
// channels that are still pending, with the concatenated per-channel result
// identical to one successful FlushMulti.
func TestFlushMulti_FailedDrainResumesOnlyRemainingChannels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inject  func(s *flakyStage)
		errIsAt string
	}{
		// Flush of the last stage fails: nothing of channel 1 was returned yet.
		{"flush", func(s *flakyStage) { s.failFlush = 1 }, "flush error"},
		// Process of input already read out of the ring buffer fails: the read
		// must be retried with the same samples, not dropped or duplicated.
		{"process", func(s *flakyStage) { s.failProcess = 1 }, "flush-process error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const channels = 3
			inputs := testChannels(channels, 4800)
			refProc, refTails := drainReference(t, channels, inputs)

			r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
			require.NoError(t, err)

			proc, err := r.ProcessMulti(cloneChannels(inputs))
			require.NoError(t, err)
			for ch := range channels {
				require.Equal(t, refProc[ch], proc[ch], "channel %d: Process output", ch)
			}

			crr, ok := r.(*constantRateResampler)
			require.True(t, ok)

			// Make the middle channel's last stage fail its first drain call.
			mid := crr.channels[1]
			last := len(mid.stages) - 1
			flaky := &flakyStage{Stage: mid.stages[last]}
			tc.inject(flaky)
			mid.stages[last] = flaky

			mf, ok := r.(MultiFlusher)
			require.True(t, ok)

			first, err := mf.FlushMulti()
			require.ErrorIs(t, err, errInjected)
			require.Contains(t, err.Error(), tc.errIsAt)
			require.Len(t, first, channels)

			// Channels that finished before the failure keep their data.
			require.Equal(t, refTails[0], first[0], "channel 0 tail lost or altered by the failed drain")
			require.Equal(t, refTails[2], first[2], "channel 2 tail lost or altered by the failed drain")
			require.Empty(t, first[1], "channel 1 should have produced nothing before the injected failure")

			// A retry completes only channel 1; the others are already done.
			second, err := mf.FlushMulti()
			require.NoError(t, err)
			require.Empty(t, second[0], "channel 0 drained twice")
			require.Empty(t, second[2], "channel 2 drained twice")
			require.Equal(t, refTails[1], second[1], "channel 1 tail differs from one-shot FlushMulti")

			// A third call is a side-effect-free empty result.
			third, err := mf.FlushMulti()
			require.NoError(t, err)
			for ch := range channels {
				require.Empty(t, third[ch], "channel %d drained again after completion", ch)
			}
		})
	}
}

// TestFlushMulti_ErrorPartialStateHasNoDuplicates checks that retrying a failed
// drain never returns samples twice: for every channel, the concatenation of
// all attempts equals the one-shot result.
func TestFlushMulti_ErrorPartialStateHasNoDuplicates(t *testing.T) {
	const channels = 3
	inputs := testChannels(channels, 2400)
	_, refTails := drainReference(t, channels, inputs)

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
	require.NoError(t, err)
	_, err = r.ProcessMulti(cloneChannels(inputs))
	require.NoError(t, err)

	crr, ok := r.(*constantRateResampler)
	require.True(t, ok)
	// Fail the first flush of every channel's first stage; each retry finishes
	// one channel, so this takes three attempts.
	for _, ch := range crr.channels {
		ch.stages[0] = &flakyStage{Stage: ch.stages[0], failFlush: 1}
	}

	mf := r.(MultiFlusher) //nolint:forcetypeassert // *constantRateResampler implements MultiFlusher
	collected := make([][]float64, channels)
	for range channels + 1 {
		out, err := mf.FlushMulti()
		if err != nil {
			require.ErrorIs(t, err, errInjected)
		}
		for ch := range channels {
			collected[ch] = slices.Concat(collected[ch], out[ch])
		}
	}

	for ch := range channels {
		require.Equal(t, refTails[ch], collected[ch],
			"channel %d: retried drains did not reproduce the one-shot tail", ch)
	}
}

// TestFlushMulti_ConcurrentDrainsSingleAdvance runs drains concurrently: only
// one may advance the state, the others must wait and then observe the
// completed, empty result. Concatenating all results must equal a single
// successful drain, with no duplicated tail.
func TestFlushMulti_ConcurrentDrainsSingleAdvance(t *testing.T) {
	const channels = 4
	inputs := testChannels(channels, 2400)
	_, refTails := drainReference(t, channels, inputs)

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, true))
	require.NoError(t, err)
	_, err = r.ProcessMulti(cloneChannels(inputs))
	require.NoError(t, err)

	mf := r.(MultiFlusher) //nolint:forcetypeassert // *constantRateResampler implements MultiFlusher

	const drainers = 4
	results := make([][][]float64, drainers)
	errs := make([]error, drainers)
	var wg sync.WaitGroup
	wg.Add(drainers)
	for i := range drainers {
		go func() {
			defer wg.Done()
			results[i], errs[i] = mf.FlushMulti()
		}()
	}
	wg.Wait()

	for i := range drainers {
		require.NoError(t, errs[i])
	}
	for ch := range channels {
		// results[i][ch] depends on the loop variable, so this is not the
		// slice-repetition idiom the ruleguard rule targets; build the parts
		// explicitly and concatenate once.
		parts := make([][]float64, drainers)
		for i := range drainers {
			parts[i] = results[i][ch]
		}
		require.Equal(t, refTails[ch], slices.Concat(parts...),
			"channel %d: concurrent drains produced a duplicated or reordered tail", ch)
	}
}

// TestMultiChannel_FlushThenFlushMultiDrainsEachChannelOnce verifies that the
// mono Flush entry point and FlushMulti share one lifecycle: Flush drains
// channel 0 only, and a following FlushMulti returns the remaining channels and
// nothing for channel 0.
func TestMultiChannel_FlushThenFlushMultiDrainsEachChannelOnce(t *testing.T) {
	const channels = 3
	inputs := testChannels(channels, 2400)
	_, refTails := drainReference(t, channels, inputs)

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
	require.NoError(t, err)
	_, err = r.ProcessMulti(cloneChannels(inputs))
	require.NoError(t, err)

	ch0, err := r.Flush()
	require.NoError(t, err)
	require.Equal(t, refTails[0], ch0)

	rest, err := r.(MultiFlusher).FlushMulti() //nolint:forcetypeassert // *constantRateResampler implements MultiFlusher
	require.NoError(t, err)
	require.Empty(t, rest[0], "channel 0 drained twice across Flush and FlushMulti")
	require.Equal(t, refTails[1], rest[1])
	require.Equal(t, refTails[2], rest[2])
}

// TestMultiChannel_ReuseAfterDrain checks that a drained instance can be used
// for another stream, either via Reset or by simply continuing with more input,
// and that the new stream is bit-identical to a freshly constructed instance and
// drains exactly once.
func TestMultiChannel_ReuseAfterDrain(t *testing.T) {
	const channels = 2
	inputs := testChannels(channels, 2400)
	freshProc, freshTails := drainReference(t, channels, inputs)

	for _, reuse := range []struct {
		name  string
		after func(r Resampler)
	}{
		{"Reset", func(r Resampler) { r.Reset() }},
		{"ProcessAfterDrain", func(Resampler) {}},
	} {
		t.Run(reuse.name, func(t *testing.T) {
			r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
			require.NoError(t, err)
			_, err = r.ProcessMulti(cloneChannels(inputs))
			require.NoError(t, err)
			mf, ok := r.(MultiFlusher)
			require.True(t, ok)
			_, err = mf.FlushMulti()
			require.NoError(t, err)

			reuse.after(r)

			proc, err := r.ProcessMulti(cloneChannels(inputs))
			require.NoError(t, err)
			tails, err := mf.FlushMulti()
			require.NoError(t, err)

			for ch := range channels {
				require.Equal(t, freshProc[ch], proc[ch], "channel %d: Process on reused instance differs from fresh", ch)
				require.Equal(t, freshTails[ch], tails[ch], "channel %d: tail on reused instance differs from fresh", ch)
			}

			again, err := mf.FlushMulti()
			require.NoError(t, err)
			for ch := range channels {
				require.Empty(t, again[ch], "channel %d drained again after completion", ch)
			}
		})
	}
}

// TestMultiChannelConcurrentProcessAndDrain exercises the lock that keeps a
// drain from interleaving with processing under the race detector. Every
// emitted sample must be finite; without the lock the stages' delay lines would
// be read and advanced concurrently.
func TestMultiChannelConcurrentProcessAndDrain(t *testing.T) {
	const channels = 3
	inputs := testChannels(channels, 4800)

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, true))
	require.NoError(t, err)
	mf, ok := r.(MultiFlusher)
	require.True(t, ok)

	var processOut, drainOut [][]float64
	var processErr, drainErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		processOut, processErr = r.ProcessMulti(cloneChannels(inputs))
	}()
	go func() {
		defer wg.Done()
		drainOut, drainErr = mf.FlushMulti()
	}()

	wg.Wait()

	require.NoError(t, processErr)
	require.NoError(t, drainErr)
	for ch := range channels {
		for _, v := range processOut[ch] {
			require.False(t, math.IsNaN(v) || math.IsInf(v, 0), "channel %d: Process emitted a non-finite sample", ch)
		}
		for _, v := range drainOut[ch] {
			require.False(t, math.IsNaN(v) || math.IsInf(v, 0), "channel %d: drain emitted a non-finite sample", ch)
		}
	}
}

// TestFlush_MonoNewPathLifecycle pins that the mono New(config) path keeps the
// engine's Flush contract while sharing the new lifecycle bookkeeping: a second
// Flush is an empty no-op, and a later Process matches a fresh instance.
func TestFlush_MonoNewPathLifecycle(t *testing.T) {
	const n = 2400
	in := make([]float64, n)
	for i := range in {
		in[i] = 0.5 * math.Sin(float64(i)*0.01)
	}

	r, err := New(newConfig(48000, 16000, QualityHigh, 1, false))
	require.NoError(t, err)
	first, err := r.Process(slices.Clone(in))
	require.NoError(t, err)
	tail, err := r.Flush()
	require.NoError(t, err)

	second, err := r.Flush()
	require.NoError(t, err)
	require.Empty(t, second, "second Flush returned samples")

	fresh, err := New(newConfig(48000, 16000, QualityHigh, 1, false))
	require.NoError(t, err)
	want, err := fresh.Process(slices.Clone(in))
	require.NoError(t, err)

	got, err := r.Process(slices.Clone(in))
	require.NoError(t, err)
	require.Equal(t, want, got, "Process after Flush differs from a fresh instance")

	// The first stream itself is unchanged by the bookkeeping.
	wantTail, err := fresh.Flush()
	require.NoError(t, err)
	require.Equal(t, append(want, wantTail...), append(first, tail...), "mono output changed")
}

// TestFlushMulti_ErrorNamesChannel keeps the error text actionable: the caller
// must be able to tell which channel stopped.
func TestFlushMulti_ErrorNamesChannel(t *testing.T) {
	const channels = 2
	inputs := testChannels(channels, 480)

	r, err := New(newConfig(48000, 16000, QualityHigh, channels, false))
	require.NoError(t, err)
	_, err = r.ProcessMulti(cloneChannels(inputs))
	require.NoError(t, err)

	crr, ok := r.(*constantRateResampler)
	require.True(t, ok)
	last := len(crr.channels[1].stages) - 1
	crr.channels[1].stages[last] = &flakyStage{Stage: crr.channels[1].stages[last], failFlush: 1}

	_, err = r.(MultiFlusher).FlushMulti() //nolint:forcetypeassert // *constantRateResampler implements MultiFlusher
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("channel %d", 1))
}
