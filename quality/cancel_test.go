package quality

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eko/qc/decode"
	"github.com/eko/qc/frame"
)

// cancellingSource decodes n frames like selectingSource, checking ctx and
// calling after every emitted frame: a test uses after to cancel the
// measurement at a chosen point, deterministic whatever the worker timing.
type cancellingSource struct {
	n     int
	after func(emitted int64)

	emitted atomic.Int64
}

func (s *cancellingSource) Decode(
	ctx context.Context,
	req decode.Request,
	fn func(*frame.Frame) error,
) error {
	output := 0

	for i := 0; req.FirstIndex+i < s.n; i++ {
		if req.MaxFrames > 0 && output == req.MaxFrames {
			break
		}

		if !selected(req.Select, i) {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		f := req.Pool.Get()
		f.Index = req.FirstIndex + i

		output++

		if s.after != nil {
			s.after(s.emitted.Add(1))
		}

		if err := fn(f); err != nil {
			return err
		}
	}

	return ctx.Err()
}

// cancelAfter returns an after hook cancelling ctx once emitted frames reach
// n.
func cancelAfter(
	cancel context.CancelFunc,
	n int64,
) func(int64) {
	return func(emitted int64) {
		if emitted == n {
			cancel()
		}
	}
}

// waitClosed waits for Measure to return, failing instead of hanging.
func waitClosed(
	t *testing.T,
	done <-chan error,
) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		require.FailNow(t, "Measure did not return after cancellation")

		return nil
	}
}

// TestMeasureCancelledSampled cancels a sampled measurement mid-decode: it
// must return the cancellation instead of a partial Result, and release the
// models and scorers it created.
func TestMeasureCancelledSampled(
	t *testing.T,
) {
	const n = 600

	engine := &fakeEngine{}
	source := &cancellingSource{n: n}

	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	source.after = cancelAfter(cancel, 5)

	opts := Options{Model: testModel, InitialClips: 8}

	done := make(chan error, 1)

	go func() {
		res, err := NewMeter(source, engine).Measure(ctx, ref, dist, opts)

		assert.Nil(t, res, "no partial result")
		done <- err
	}()

	err := waitClosed(t, done)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// slowSource decodes n frames but waits delay before every frame unless ctx
// is done: a cancellation must cut that wait short instead of letting the
// decode run to its end.
type slowSource struct {
	n     int
	delay time.Duration
}

func (s *slowSource) Decode(
	ctx context.Context,
	req decode.Request,
	fn func(*frame.Frame) error,
) error {
	output := 0

	for i := 0; req.FirstIndex+i < s.n; i++ {
		if req.MaxFrames > 0 && output == req.MaxFrames {
			break
		}

		if !selected(req.Select, i) {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.delay):
		}

		f := req.Pool.Get()
		f.Index = req.FirstIndex + i

		output++

		if err := fn(f); err != nil {
			return err
		}
	}

	return ctx.Err()
}

// TestMeasureCancelledReturnsPromptly cancels a slow measurement and checks
// that it returns well before the decode would have finished, and that no
// Progress callback runs after the return.
func TestMeasureCancelledReturnsPromptly(
	t *testing.T,
) {
	const n = 4000

	source := &slowSource{n: n, delay: 5 * time.Millisecond}
	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())

	var after atomic.Int32

	opts := Options{
		Model:        testModel,
		InitialClips: 8,
		Progress: func(Progress) {
			if ctx.Err() != nil {
				after.Add(1)
			}
		},
	}

	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	started := time.Now()

	res, err := NewMeter(source, &fakeEngine{}).Measure(ctx, ref, dist, opts)

	assert.Less(t, time.Since(started), time.Second, "returned well before decoding every frame would take")
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, after.Load(), "no callback runs once the measurement is cancelled")
}

// TestMeasureCancelledDrainsPendingClips queues more clips than the workers
// can score and cancels: the queued clips' frames must be released (the
// sweep's drain), so the measurement cannot hang, and it returns.
func TestMeasureCancelledDrainsPendingClips(
	t *testing.T,
) {
	const n = 4000

	engine := &fakeEngine{}
	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var cancels atomic.Int32

	opts := Options{
		Model:        testModel,
		Sample:       Sample{Share: 0.5},
		Workers:      1,
		InitialClips: 8,
		Progress: func(Progress) {
			if cancels.Add(1) == 3 {
				cancel()
			}
		},
	}

	done := make(chan error, 1)

	go func() {
		res, err := NewMeter(&cancellingSource{n: n}, engine).Measure(ctx, ref, dist, opts)

		assert.Nil(t, res)
		done <- err
	}()

	require.ErrorIs(t, waitClosed(t, done), context.Canceled)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestMeasureCancelledFixedBudget cancels a fixed-budget measurement before
// its budget is spent: the partial budget never stands for a complete one.
func TestMeasureCancelledFixedBudget(
	t *testing.T,
) {
	const n = 600

	engine := &fakeEngine{}
	source := &cancellingSource{n: n}

	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	source.after = cancelAfter(cancel, 5)

	opts := Options{Model: testModel, Sample: Sample{Share: 0.2}}

	done := make(chan error, 1)

	go func() {
		res, err := NewMeter(source, engine).Measure(ctx, ref, dist, opts)

		assert.Nil(t, res, "a cancelled budget is not a budget spent")
		done <- err
	}()

	err := waitClosed(t, done)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestMeasureCancelledExactSegments cancels an exact measurement scored in
// concurrent segments: every segment's decoder must stop, and no mean is
// merged from the segments that completed.
func TestMeasureCancelledExactSegments(
	t *testing.T,
) {
	const n = 3000

	engine := &fakeEngine{}
	slow := &cancellingSource{n: n}
	source := &segmentingSource{Source: slow, decoders: 12}

	ref, dist := fakeInput("ref.mp4", n, 250), fakeInput("dist.mp4", n, 250)

	ctx, cancel := context.WithCancel(t.Context())
	slow.after = cancelAfter(cancel, 9)

	done := make(chan error, 1)

	go func() {
		res, err := NewMeter(source, engine).Measure(ctx, ref, dist, Options{Model: testModel, Exact: true})

		assert.Nil(t, res, "no mean over missing segments")
		done <- err
	}()

	err := waitClosed(t, done)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestMeasureCancelDuringLaterRound cancels a precision-driven measurement
// while a later round is scoring: the half-scored round is never recorded,
// and no Result is reported.
func TestMeasureCancelDuringLaterRound(
	t *testing.T,
) {
	const n = 600

	engine := &fakeEngine{}
	source := selectingSource(n)

	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		cancelled atomic.Bool
		late      atomic.Int32
	)

	opts := Options{
		Model:        testModel,
		Precision:    1e-6,
		Budget:       true,
		MaxShare:     0.9,
		InitialClips: 4,
		Workers:      2,
		Progress: func(p Progress) {
			if cancelled.Load() {
				late.Add(1)

				return
			}

			// Round 2 is under way (per-clip reports are not estimates):
			// cancel in the middle of it.
			if p.Round >= 2 && !p.Estimated {
				cancelled.Store(true)
				cancel()
			}
		},
	}

	res, err := NewMeter(source, engine).Measure(ctx, ref, dist, opts)

	require.True(t, cancelled.Load(), "the test reached a second round")
	assert.Nil(t, res, "no Result from a partly scored round")
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, late.Load(), "no callback starts once the measurement is cancelled")
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestMeasureCancelBetweenRounds cancels once a round has completed, before
// the next one is planned: the measurement stops there.
func TestMeasureCancelBetweenRounds(
	t *testing.T,
) {
	const n = 600

	engine := &fakeEngine{}
	source := selectingSource(n)

	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var cancelled atomic.Bool

	opts := Options{
		Model:        testModel,
		Precision:    1e-6,
		Budget:       true,
		MaxShare:     0.9,
		InitialClips: 4,
		Progress: func(p Progress) {
			// The first round's estimate: another round is needed, and the
			// test cancels before it starts.
			if p.Round == 1 && p.Estimated {
				cancelled.Store(true)
				cancel()
			}
		},
	}

	res, err := NewMeter(source, engine).Measure(ctx, ref, dist, opts)

	require.True(t, cancelled.Load(), "the test reached the first estimate")
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestMeasureCancelledReleasesFrames checks that a cancelled measurement
// leaves nothing held: a second measurement on the same Meter, sharing its
// frame pools and memory budget, finds the budget free and completes.
func TestMeasureCancelledReleasesFrames(
	t *testing.T,
) {
	const n = 600

	engine := &fakeEngine{}
	ref, dist := fakeInput("ref.mp4", n, 50), fakeInput("dist.mp4", n, 50)

	ctx, cancel := context.WithCancel(t.Context())
	source := &cancellingSource{n: n, after: cancelAfter(cancel, 5)}

	// A budget of one unit per acquisition: without the cancelled workers
	// giving theirs back, the second measurement would wait for it forever.
	meter := NewMeter(source, engine, WithMemory(1))

	_, err := meter.Measure(ctx, ref, dist, Options{Model: testModel, InitialClips: 8, Workers: 3})
	require.ErrorIs(t, err, context.Canceled)

	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel2()

	res, err := meter.Measure(ctx2, ref, dist, Options{Model: testModel, Precision: 1000, InitialClips: 8, Workers: 3})
	require.NoError(t, err, "the memory budget is free again")
	require.NotNil(t, res)
	assert.Equal(t, ModeSampled, res.Mode)
	assert.Positive(t, res.FramesScored)
	assert.Zero(t, engine.open.Load(), "models and scorers are closed")
}

// TestNotifyCancelled covers the guarantees of Progress reporting: no new
// callback starts once the measurement is cancelled (one running is allowed
// to finish), and FramesScored never goes backwards.
func TestNotifyCancelled(
	t *testing.T,
) {
	t.Run("no callback after cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		var calls atomic.Int32

		r := &run{
			ctx:  ctx,
			opts: Options{Progress: func(Progress) { calls.Add(1) }},
		}

		r.notify(Progress{FramesScored: 1})
		require.Equal(t, int32(1), calls.Load())

		cancel()

		r.notify(Progress{FramesScored: 2})
		assert.Equal(t, int32(1), calls.Load(), "no callback after the cancellation")
	})

	t.Run("a running callback finishes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		finished := make(chan struct{})

		r := &run{
			ctx: ctx,
			opts: Options{Progress: func(Progress) {
				cancel()
				close(finished)
			}},
		}

		r.notify(Progress{FramesScored: 1})

		select {
		case <-finished:
		default:
			require.FailNow(t, "the callback in progress did not finish")
		}

		// A caller that returns waits for it, then no callback runs.
		r.waitReports()
	})

	t.Run("FramesScored never goes backwards", func(t *testing.T) {
		var (
			mu   sync.Mutex
			seen []int
		)

		r := &run{opts: Options{Progress: func(p Progress) {
			mu.Lock()
			seen = append(seen, p.FramesScored)
			mu.Unlock()
		}}}

		r.notify(Progress{FramesScored: 5})
		r.notify(Progress{FramesScored: 3})
		r.notify(Progress{FramesScored: 8})

		assert.Equal(t, []int{5, 5, 8}, seen)
	})
}

// TestCheckSegments rejects a merge that would report a mean over missing or
// truncated segments.
func TestCheckSegments(
	t *testing.T,
) {
	st := newStratum(0, 10, 10)
	clips := []clip{
		{stratum: st, from: 0, to: 4},
		{stratum: st, from: 4, to: 10},
	}

	full := func() []clipResult {
		return []clipResult{
			{clip: clips[0], scores: []float64{1, 2, 3, 4}},
			{clip: clips[1], scores: []float64{5, 6, 7, 8, 9, 10}},
		}
	}

	missing := full()[:1]

	truncated := full()
	truncated[1].scores = truncated[1].scores[:2]

	hole := full()
	hole[1].clip.from = 5

	testCases := []struct {
		name    string
		results []clipResult
		wantErr bool
	}{
		{name: "complete", results: full()},
		{name: "a segment missing", results: missing, wantErr: true},
		{name: "a segment truncated", results: truncated, wantErr: true},
		{name: "a gap between segments", results: hole, wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := checkSegments(clips, testCase.results, 10)
			if testCase.wantErr {
				require.ErrorIs(t, err, errMissingSegments)

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestSampleDropsCancelledRound checks that the sampling loop never records
// a round whose scoring failed: a cancellation mid-round leaves the previous
// rounds' results only.
func TestSampleDropsCancelledRound(
	t *testing.T,
) {
	errCancel := errors.New("cancelled round")

	strata := uniformStrata(4000, 400)
	opts := Options{Precision: 1e-6, Budget: true, MaxShare: 0.9, InitialClips: 4}.withDefaults()

	calls := 0
	score := func(_ context.Context, clips []clip, round int) ([]clipResult, error) {
		calls++

		if round == 1 {
			return scoresBy(noisy(4))(context.Background(), clips, round)
		}

		return nil, errCancel
	}

	out, err := sample(t.Context(), strata, 4000, opts, score, nil)
	require.ErrorIs(t, err, errCancel)
	require.Greater(t, calls, 1, "the loop reached a second round")

	// Only the first round's clips are recorded: the failed round added none.
	scored := 0
	for _, s := range strata {
		scored += len(s.clipMeans)
	}

	assert.Equal(t, scored, len(out.results), "no clip of the cancelled round is recorded")
	assert.Equal(t, 1, out.rounds)
}

// TestSampleBudgetDropsCancelledRound checks that a cancelled fixed budget
// records nothing and reports no estimate.
func TestSampleBudgetDropsCancelledRound(
	t *testing.T,
) {
	errCancel := errors.New("cancelled budget")

	pts := framesPTS(4000)
	opts := Options{Sample: Sample{Share: 0.05}, Precision: 1e-9}.withDefaults()

	score := func(context.Context, []clip, int) ([]clipResult, error) {
		return nil, errCancel
	}

	strata, out, err := sampling(t.Context(), 4000, pts, nil, opts, score, nil)
	require.ErrorIs(t, err, errCancel)
	require.NotEmpty(t, strata)

	assert.Empty(t, out.results, "a cancelled budget records no clip")
	assert.Zero(t, out.scored)

	for _, s := range strata {
		assert.Empty(t, s.clipMeans)
	}
}

// TestMeasureCancelledBeforeStart returns the cancellation without decoding
// anything when ctx is already done.
func TestMeasureCancelledBeforeStart(
	t *testing.T,
) {
	engine := &fakeEngine{}
	source := &cancellingSource{n: 600, after: func(int64) {
		require.FailNow(t, "no frame is decoded once ctx is cancelled")
	}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ref, dist := fakeInput("ref.mp4", 600, 50), fakeInput("dist.mp4", 600, 50)

	res, err := NewMeter(source, engine).Measure(ctx, ref, dist, Options{Model: testModel})

	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, engine.open.Load())
}
