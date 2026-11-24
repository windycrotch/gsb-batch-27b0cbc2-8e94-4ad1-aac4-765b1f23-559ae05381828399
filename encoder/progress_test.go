package encoder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/farshidrezaei/mosaic/config"
	"github.com/farshidrezaei/mosaic/internal/executor"
	"github.com/farshidrezaei/mosaic/ladder"
	"github.com/farshidrezaei/mosaic/probe"
)

// scriptedExecutor drives an executor from a callback, so tests can reproduce
// exact progress/cancellation/failure timings without real FFmpeg.
type scriptedExecutor struct {
	run func(ctx context.Context, progress chan<- string) (*executor.Usage, error)
}

func (s *scriptedExecutor) Execute(ctx context.Context, _ string, _ ...string) ([]byte, *executor.Usage, error) {
	return s.ExecuteWithProgress(ctx, nil, "ffmpeg")
}

func (s *scriptedExecutor) ExecuteWithProgress(ctx context.Context, progress chan<- string, _ string, _ ...string) ([]byte, *executor.Usage, error) {
	if progress != nil {
		defer close(progress)
	}
	usage, err := s.run(ctx, progress)
	return nil, usage, err
}

func encodeTestInputs() (probe.VideoInfo, config.Profile, []ladder.Rendition) {
	return probe.VideoInfo{Width: 640, Height: 360, FPS: 30, Duration: 10, HasAudio: false},
		config.VOD,
		[]ladder.Rendition{{Width: 640, Height: 360, MaxRate: 1000, BufSize: 2000, Profile: "baseline", Level: "3.0"}}
}

func TestRunEncodingCommand_DeliversFinalProgressOnSuccess(t *testing.T) {
	info, profile, l := encodeTestInputs()
	mock := &scriptedExecutor{run: func(_ context.Context, progress chan<- string) (*executor.Usage, error) {
		progress <- "frame=10\nout_time=00:00:02.000000\nprogress=continue\n"
		progress <- "frame=20\nout_time=00:00:05.000000\nprogress=continue\n"
		progress <- "frame=30\nout_time=00:00:10.000000\nprogress=end\n"
		return &executor.Usage{UserTime: 3}, nil
	}}

	var mu sync.Mutex
	var snapshots []map[string]string
	handler := func(m map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		snapshots = append(snapshots, m)
	}

	usage, err := EncodeHLSCMAFWithExecutor(context.Background(), "in.mp4", t.TempDir(), info, profile, l, mock, handler, EncoderOptions{})
	if err != nil {
		t.Fatalf("EncodeHLSCMAFWithExecutor() err=%v", err)
	}
	if usage == nil || usage.UserTime != 3 {
		t.Fatalf("unexpected usage: %+v", usage)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 progress updates, got %d", len(snapshots))
	}
	last := snapshots[len(snapshots)-1]
	if last["progress"] != "end" {
		t.Fatalf("expected final progress=end, got %+v", last)
	}
}

func TestRunEncodingCommand_CancelReturnsPromptlyWithBlockedHandler(t *testing.T) {
	info, profile, l := encodeTestInputs()

	firstCall := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls int32

	handler := func(map[string]string) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(firstCall)
			<-release // simulate a slow / blocking progress consumer
		}
	}

	mock := &scriptedExecutor{run: func(ctx context.Context, progress chan<- string) (*executor.Usage, error) {
		for i := 0; i < 1000; i++ {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case progress <- fmt.Sprintf("frame=%d\nout_time=00:00:01.000000\nprogress=continue\n", i):
			}
		}
		return &executor.Usage{}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := EncodeHLSCMAFWithExecutor(ctx, "in.mp4", t.TempDir(), info, profile, l, mock, handler, EncoderOptions{})
		done <- err
	}()

	select {
	case <-firstCall:
	case <-time.After(2 * time.Second):
		t.Fatal("progress handler was never invoked")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("encode did not return promptly after cancellation; progress callback blocked it")
	}

	afterReturn := atomic.LoadInt32(&calls)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != afterReturn {
		t.Fatalf("progress handler kept being called after cancellation: %d -> %d", afterReturn, got)
	}
}

func TestRunEncodingCommand_PreservesCommandErrorWithStderr(t *testing.T) {
	info, profile, l := encodeTestInputs()
	cmdErr := &executor.CommandError{
		Command: "ffmpeg",
		Args:    []string{"-i", "in.mp4"},
		Err:     errors.New("exit status 1"),
		Stderr:  "Invalid data found when processing input",
	}

	mock := &scriptedExecutor{run: func(_ context.Context, progress chan<- string) (*executor.Usage, error) {
		progress <- "frame=1\nprogress=continue\n"
		return nil, cmdErr
	}}

	var calls int32
	handler := func(map[string]string) { atomic.AddInt32(&calls, 1) }

	_, err := EncodeDASHCMAFWithExecutor(context.Background(), "in.mp4", t.TempDir(), info, profile, l, mock, handler, EncoderOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.As(err, new(*executor.CommandError)) {
		t.Fatalf("expected CommandError in chain, got %T (%v)", err, err)
	}
	var got *executor.CommandError
	if !errors.As(err, &got) || got.Stderr != cmdErr.Stderr || got.Command != "ffmpeg" {
		t.Fatalf("command/stderr context lost: %+v", got)
	}
	if !strings.Contains(err.Error(), "Invalid data found when processing input") {
		t.Fatalf("error string lacks stderr clue: %v", err)
	}
}

func TestRunEncodingCommand_CancelBeatsRacingCommandError(t *testing.T) {
	info, profile, l := encodeTestInputs()
	ctx, cancel := context.WithCancel(context.Background())

	mock := &scriptedExecutor{run: func(ctx context.Context, progress chan<- string) (*executor.Usage, error) {
		cancel() // cancellation lands before the command result is processed
		return nil, errors.New("signal: killed")
	}}

	_, err := EncodeHLSCMAFWithExecutor(ctx, "in.mp4", t.TempDir(), info, profile, l, mock, func(map[string]string) {}, EncoderOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRunEncodingCommand_CancelWithoutProgressHandler(t *testing.T) {
	info, profile, l := encodeTestInputs()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mock := &scriptedExecutor{run: func(ctx context.Context, _ chan<- string) (*executor.Usage, error) {
		return nil, ctx.Err()
	}}

	_, err := EncodeHLSCMAFWithExecutor(ctx, "in.mp4", t.TempDir(), info, profile, l, mock, nil, EncoderOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRunEncodingCommand_ConcurrentJobsDoNotCrossTalk(t *testing.T) {
	const jobs = 6
	var wg sync.WaitGroup
	errs := make(chan error, jobs)

	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			info, profile, l := encodeTestInputs()

			var mu sync.Mutex
			var seen []string
			handler := func(m map[string]string) {
				mu.Lock()
				seen = append(seen, m["job"])
				mu.Unlock()
			}

			tag := fmt.Sprintf("job-%d", idx)
			mock := &scriptedExecutor{run: func(_ context.Context, progress chan<- string) (*executor.Usage, error) {
				for i := 0; i < 5; i++ {
					progress <- fmt.Sprintf("job=%s\nframe=%d\nout_time=00:00:0%d.000000\nprogress=continue\n", tag, i, i)
				}
				progress <- fmt.Sprintf("job=%s\nout_time=00:00:10.000000\nprogress=end\n", tag)
				return &executor.Usage{UserTime: float64(idx)}, nil
			}}

			usage, err := EncodeHLSCMAFWithExecutor(context.Background(), "in.mp4", t.TempDir(), info, profile, l, mock, handler, EncoderOptions{})
			if err != nil {
				errs <- err
				return
			}
			if usage == nil || usage.UserTime != float64(idx) {
				errs <- fmt.Errorf("%s: usage leaked from another job: %+v", tag, usage)
				return
			}

			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 6 {
				errs <- fmt.Errorf("%s: expected 6 updates, got %d", tag, len(seen))
				return
			}
			for _, v := range seen {
				if v != tag {
					errs <- fmt.Errorf("%s: received progress from %q", tag, v)
					return
				}
			}
		}(j)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
