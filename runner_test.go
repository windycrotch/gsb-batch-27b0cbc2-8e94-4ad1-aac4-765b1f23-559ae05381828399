package spireav_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiretechnology/spireav"
	"github.com/spiretechnology/spireav/filter"
	"github.com/spiretechnology/spireav/filter/expr"
	"github.com/spiretechnology/spireav/output"
)

// TestMain lets the test binary double as a fake ffmpeg executable: when
// SPIREAV_FAKE_FFMPEG=1 is set in the environment, the binary behaves according to
// SPIREAV_FAKE_FFMPEG_MODE instead of running the test suite. This intercept happens
// before flag parsing so that arbitrary ffmpeg arguments are tolerated.
func TestMain(m *testing.M) {
	if os.Getenv("SPIREAV_FAKE_FFMPEG") == "1" {
		os.Exit(runFakeFFmpeg())
	}
	os.Exit(m.Run())
}

// runFakeFFmpeg emulates ffmpeg's machine-readable progress output on stdout and
// error logs on stderr, controlled by SPIREAV_FAKE_FFMPEG_MODE:
//
//	"success" - emit several progress updates ending with progress=end, exit 0
//	"fail"    - emit one progress update, log an error to stderr, exit 3
//	"hang"    - emit one progress update, then sleep (to be killed by cancellation)
func runFakeFFmpeg() int {
	switch os.Getenv("SPIREAV_FAKE_FFMPEG_MODE") {
	case "success":
		fmt.Fprint(os.Stdout, "frame=1\nfps=25.0\nspeed=1.0x\nprogress=continue\n")
		fmt.Fprint(os.Stdout, "frame=2\nfps=25.0\nspeed=1.0x\nprogress=continue\n")
		fmt.Fprint(os.Stdout, "frame=3\nfps=25.0\nspeed=1.0x\nprogress=end\n")
		return 0
	case "fail":
		fmt.Fprint(os.Stdout, "frame=1\nfps=25.0\nprogress=continue\n")
		fmt.Fprint(os.Stderr, "fake ffmpeg failure\n")
		return 3
	case "hang":
		fmt.Fprint(os.Stdout, "frame=1\nfps=25.0\nprogress=continue\n")
		time.Sleep(15 * time.Second)
		return 0
	}
	return 0
}

// useFakeFFmpeg points the runner at this test binary acting as ffmpeg.
func useFakeFFmpeg(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("SPIREAV_FAKE_FFMPEG", "1")
	t.Setenv("SPIREAV_FAKE_FFMPEG_MODE", mode)
	t.Cleanup(overrideFfmpegPath(os.Args[0]))
}

// progressRecorder collects progress callbacks safely from the parser goroutine.
type progressRecorder struct {
	mu         sync.Mutex
	progresses []spireav.Progress
	onCallback func()
}

func (r *progressRecorder) callback() func(spireav.Progress) {
	return func(p spireav.Progress) {
		r.mu.Lock()
		r.progresses = append(r.progresses, p)
		r.mu.Unlock()
		if r.onCallback != nil {
			r.onCallback()
		}
	}
}

func (r *progressRecorder) snapshot() []spireav.Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]spireav.Progress(nil), r.progresses...)
}

func (r *progressRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.progresses)
}

func overrideFfmpegPath(path string) func() {
	original := spireav.FfmpegPath
	spireav.FfmpegPath = path
	return func() {
		spireav.FfmpegPath = original
	}
}

func requireError(t *testing.T, err error, msgAndArgs ...interface{}) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil: %v", msgAndArgs)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func requireErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got: %v", substr, err)
	}
}

func requireEqual[T comparable](t *testing.T, expected, actual T, msgAndArgs ...interface{}) {
	t.Helper()
	if expected != actual {
		t.Fatalf("expected %v, got %v: %v", expected, actual, msgAndArgs)
	}
}

func requireTrue(t *testing.T, cond bool, msgAndArgs ...interface{}) {
	t.Helper()
	if !cond {
		t.Fatalf("expected condition to be true: %v", msgAndArgs)
	}
}

func TestRunnerErrors(t *testing.T) {
	t.Run("invalid args", func(t *testing.T) {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			t.Skip("ffmpeg is not installed on this machine")
		}
		runner := spireav.NewRunner(spireav.RunnableArgs{"several", "bad", "args"})
		err := runner.Run(context.Background())
		requireError(t, err, "expected error when running process with invalid args")
		requireErrorContains(t, err, "Invalid argument")
	})
	t.Run("ffmpeg not in path", func(t *testing.T) {
		defer overrideFfmpegPath("something-that-does-not-exist")()
		runner := spireav.NewRunner(spireav.RunnableArgs{"-i", "something"})
		err := runner.Run(context.Background())
		requireError(t, err, "expected error when path is missing ffmpeg binary")
		requireErrorContains(t, err, "starting ffmpeg process: exec: \"something-that-does-not-exist\": executable file not found")
	})
}

func TestRunnerSuccessProgress(t *testing.T) {
	useFakeFFmpeg(t, "success")

	recorder := &progressRecorder{}
	runner := spireav.NewRunner(
		spireav.RunnableArgs{"-i", "input.mp4", "output.mp4"},
		spireav.WithProgressCallback(recorder.callback()),
	)
	requireNoError(t, runner.Run(context.Background()))

	progresses := recorder.snapshot()
	requireTrue(t, len(progresses) > 0, "expected progress callbacks")

	// Progress frames must advance monotonically in the order ffmpeg emitted them,
	// and only the final callback may mark completion
	var lastFrame int64
	doneCount := 0
	for i, p := range progresses {
		isFinal := i == len(progresses)-1
		if !isFinal && p.Done {
			t.Fatalf("callback %d marked Done before the final callback", i)
		}
		if p.Frame < lastFrame {
			t.Fatalf("frames must be monotonically non-decreasing: callback %d has frame %d after %d", i, p.Frame, lastFrame)
		}
		lastFrame = p.Frame
		if p.Done {
			doneCount++
		}
	}

	// The final callback must be the one and only completion marker
	final := progresses[len(progresses)-1]
	requireTrue(t, final.Done, "final callback must mark completion")
	requireEqual(t, int64(3), final.Frame)
	requireEqual(t, 1, doneCount, "exactly one completion callback expected")
}

func TestRunnerFailureError(t *testing.T) {
	useFakeFFmpeg(t, "fail")

	recorder := &progressRecorder{}
	runner := spireav.NewRunner(
		spireav.RunnableArgs{"-i", "input.mp4", "output.mp4"},
		spireav.WithProgressCallback(recorder.callback()),
	)
	err := runner.Run(context.Background())
	requireError(t, err)

	// The error must be an ffmpeg failure, distinguishable from cancellation
	var ffmpegErr *spireav.FfmpegError
	requireTrue(t, errors.As(err, &ffmpegErr), "expected *spireav.FfmpegError, got: %v", err)
	requireEqual(t, 3, ffmpegErr.ExitCode)
	requireTrue(t, containsString(ffmpegErr.Logs, "fake ffmpeg failure"),
		"expected logs to contain the ffmpeg error, got: %v", ffmpegErr.Logs)
	requireTrue(t, !errors.Is(err, context.Canceled), "failure must not be reported as cancellation")
	requireTrue(t, !errors.Is(err, context.DeadlineExceeded), "failure must not be reported as deadline")

	// No callback may mark completion for a failed run
	for _, p := range recorder.snapshot() {
		requireTrue(t, !p.Done, "no Done callback may be delivered for a failed run")
	}
}

func TestRunnerCancellation(t *testing.T) {
	useFakeFFmpeg(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recorder := &progressRecorder{
		// Cancel while the first callback is still executing
		onCallback: cancel,
	}
	runner := spireav.NewRunner(
		spireav.RunnableArgs{"-i", "input.mp4", "output.mp4"},
		spireav.WithProgressCallback(recorder.callback()),
	)

	start := time.Now()
	err := runner.Run(ctx)
	elapsed := time.Since(start)

	// Cancellation must be distinguishable from an ffmpeg failure
	requireTrue(t, errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
	var ffmpegErr *spireav.FfmpegError
	requireTrue(t, !errors.As(err, &ffmpegErr), "cancellation must not surface as an ffmpeg error")

	// Run must return promptly, not wait for the hung process's natural lifetime
	requireTrue(t, elapsed < 10*time.Second, "Run took too long to return after cancellation: %v", elapsed)

	// No updates after cancellation: only the single in-flight callback was delivered,
	// and it may not mark completion
	progresses := recorder.snapshot()
	requireEqual(t, 1, len(progresses), "no progress updates may be delivered after cancellation")
	requireTrue(t, !progresses[0].Done, "no Done callback may be delivered for a canceled run")

	// No callback may fire after Run has returned
	countAfterRun := recorder.count()
	time.Sleep(150 * time.Millisecond)
	requireEqual(t, countAfterRun, recorder.count(), "no callbacks may be delivered after Run returns")
}

func TestGraphRunnableArgsDeterministic(t *testing.T) {
	useFakeFFmpeg(t, "success")

	// Build a graph whose filter carries several options
	g := spireav.New()
	inputNode := g.Input("input.mp4")
	outputNode := g.Output("output.mp4", output.WithFormatMP4())
	scale := g.Filter(filter.New("scale", 1,
		filter.WithOption("w", expr.Int(640)),
		filter.WithOption("h", expr.Int(480)),
		filter.WithOption("flags", expr.Raw("lanczos")),
		filter.WithOption("interl", expr.Int(0)),
	))
	inputNode.Video(0).Pipe(scale, 0)
	scale.Pipe(outputNode, 0)

	// Generate the args many times: they must never vary
	first, err := g.RunnableArgs()
	requireNoError(t, err)
	for i := 0; i < 100; i++ {
		again, err := g.RunnableArgs()
		requireNoError(t, err)
		requireTrue(t, reflect.DeepEqual(first, again),
			"args must be identical on every generation:\nfirst: %v\ngot:   %v", first, again)
	}

	// Execute the graph twice; execution must not change the generated command
	for run := 0; run < 2; run++ {
		requireNoError(t, spireav.NewRunner(g).Run(context.Background()))
		afterRun, err := g.RunnableArgs()
		requireNoError(t, err)
		requireTrue(t, reflect.DeepEqual(first, afterRun),
			"args must not change after execution (run %d):\nfirst: %v\ngot:   %v", run, first, afterRun)

		cmdStr, err := spireav.NewRunner(g).CommandString()
		requireNoError(t, err)
		requireTrue(t, strings.Contains(cmdStr, "scale=w=640:h=480:flags=lanczos:interl=0"),
			"filter options must be rendered in insertion order, got: %s", cmdStr)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
