package mosaic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/farshidrezaei/mosaic/internal/executor"
)

// cancelTestExecutor simulates the full command sequence used by the encode
// entry points (orientation probe/normalize, video+audio probe, then encode)
// and lets each test decide how the encode command behaves.
type cancelTestExecutor struct {
	// encode runs in place of the final `ffmpeg ... -progress pipe:1` call.
	encode func(ctx context.Context, progress chan<- string) error
	// normalizeCreatesOutput makes the orientation ffmpeg step touch its
	// output path, mimicking a real normalized file.
	normalizeCreatesOutput bool
	// normalizeErr fails the orientation normalization step.
	normalizeErr error
	// outDir is where a simulated FFmpeg writes partial output.
	outDir string
	// encodeWrites makes the encode step write these files into outDir before
	// running encode, mimicking partial segments.
	encodeWrites []string

	mu          sync.Mutex
	ffmpegCalls int
}

func (m *cancelTestExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, *executor.Usage, error) {
	return m.ExecuteWithProgress(ctx, nil, name, args...)
}

func (m *cancelTestExecutor) ExecuteWithProgress(ctx context.Context, progress chan<- string, name string, args ...string) ([]byte, *executor.Usage, error) {
	if name == "ffprobe" {
		// First probe (orientation) returns rotation, later probes are the
		// regular video/audio probes.
		if hasArg(args, "stream=width,height,codec_name:stream_tags=rotate") {
			return []byte(`{"streams":[{"width":1920,"height":1080,"codec_name":"h264","side_data_list":[{"rotation":90}]}]}`), nil, nil
		}
		if hasArg(args, "a:0") {
			return []byte("0"), nil, nil
		}
		return []byte(`{"streams":[{"width":1920,"height":1080,"avg_frame_rate":"30/1","duration":"10.0"}],"format":{"duration":"10.0"}}`), nil, nil
	}

	if name != "ffmpeg" {
		return nil, nil, fmt.Errorf("unexpected command: %s", name)
	}

	m.mu.Lock()
	m.ffmpegCalls++
	m.mu.Unlock()

	if progress != nil {
		defer close(progress)
	}

	// The orientation normalization step is the only ffmpeg call without the
	// encode filter graph.
	if !hasArg(args, "-filter_complex") {
		if m.normalizeErr != nil {
			return nil, nil, m.normalizeErr
		}
		if m.normalizeCreatesOutput && len(args) > 0 {
			_ = os.WriteFile(args[len(args)-1], []byte("normalized"), 0o644)
		}
		return nil, nil, nil
	}

	if m.encode == nil {
		return nil, nil, nil
	}

	// Mimic FFmpeg writing partial segments before it fails or is killed.
	if len(m.encodeWrites) > 0 && m.outDir != "" {
		for _, name := range m.encodeWrites {
			_ = os.WriteFile(filepath.Join(m.outDir, name), []byte("partial"), 0o644)
		}
	}

	err := m.encode(ctx, progress)
	if err != nil {
		return nil, nil, err
	}
	return nil, &executor.Usage{UserTime: 1}, nil
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func tempDirIsEmpty(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

type encodeCase struct {
	name   string
	encode func(ctx context.Context, job Job, exec *cancelTestExecutor) (*executor.Usage, error)
}

func encodeCases() []encodeCase {
	return []encodeCase{
		{
			name: "hls",
			encode: func(ctx context.Context, job Job, exec *cancelTestExecutor) (*executor.Usage, error) {
				return EncodeHlsWithExecutor(ctx, job, exec, WithNormalizeOrientation())
			},
		},
		{
			name: "dash",
			encode: func(ctx context.Context, job Job, exec *cancelTestExecutor) (*executor.Usage, error) {
				return EncodeDashWithExecutor(ctx, job, exec, WithNormalizeOrientation())
			},
		},
	}
}

// TestEncodersCancelDeletesTempNormalizedInput verifies both packaging paths
// return the cancellation error, stop progress callbacks, and remove the
// temporary normalized input file.
func TestEncodersCancelDeletesTempNormalizedInput(t *testing.T) {
	for _, tc := range encodeCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpRoot := t.TempDir()
			t.Setenv("TMPDIR", tmpRoot)
			t.Setenv("TMP", tmpRoot)
			t.Setenv("TEMP", tmpRoot)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			firstProgress := make(chan struct{})
			var progressCalls int32
			var once sync.Once

			jobOutputDir := filepath.Join(t.TempDir(), "out")
			exec := &cancelTestExecutor{
				normalizeCreatesOutput: true,
				outDir:                 jobOutputDir,
				encodeWrites:           []string{"seg_0_0.m4s", "stream_0.m3u8"},
				encode: func(ctx context.Context, progress chan<- string) error {
					for i := 0; i < 100000; i++ {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case progress <- fmt.Sprintf("frame=%d\nout_time=00:00:01.000000\nprogress=continue\n", i):
						}
					}
					return nil
				},
			}

			job := Job{
				Input:     filepath.Join(t.TempDir(), "in.mp4"),
				OutputDir: jobOutputDir,
				Profile:   ProfileVOD,
				ProgressHandler: func(ProgressInfo) {
					atomic.AddInt32(&progressCalls, 1)
					once.Do(func() { close(firstProgress) })
					cancel()
				},
			}

			done := make(chan error, 1)
			go func() {
				_, err := tc.encode(ctx, job, exec)
				done <- err
			}()

			select {
			case <-firstProgress:
			case <-time.After(3 * time.Second):
				t.Fatal("progress handler never invoked")
			}

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("encode did not return after cancellation")
			}

			finalCalls := atomic.LoadInt32(&progressCalls)
			time.Sleep(50 * time.Millisecond)
			if got := atomic.LoadInt32(&progressCalls); got != finalCalls {
				t.Fatalf("progress kept firing after cancellation: %d -> %d", finalCalls, got)
			}
			if finalCalls != 1 {
				t.Fatalf("expected exactly one progress callback before cancellation, got %d", finalCalls)
			}

			if names := tempDirIsEmpty(t, tmpRoot); len(names) != 0 {
				t.Fatalf("temp normalized input not cleaned up: %v", names)
			}

			// Partial segments from the cancelled run must be removed.
			entries, err := os.ReadDir(jobOutputDir)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read output dir: %v", err)
			}
			if len(entries) != 0 {
				var outNames []string
				for _, e := range entries {
					outNames = append(outNames, e.Name())
				}
				t.Fatalf("expected no partial output after cancellation, got %v", outNames)
			}
		})
	}
}

// TestEncodersFFmpegFailureDeletesTempNormalizedInput verifies failure error
// context survives and the temp normalized input is removed on both paths.
func TestEncodersFFmpegFailureDeletesTempNormalizedInput(t *testing.T) {
	cmdErr := &executor.CommandError{
		Command: "ffmpeg",
		Args:    []string{"-i", "in.mp4"},
		Err:     errors.New("exit status 1"),
		Stderr:  "Invalid data found when processing input",
	}

	for _, tc := range encodeCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpRoot := t.TempDir()
			t.Setenv("TMPDIR", tmpRoot)
			t.Setenv("TMP", tmpRoot)
			t.Setenv("TEMP", tmpRoot)

			jobOutputDir := filepath.Join(t.TempDir(), "out")
			exec := &cancelTestExecutor{
				normalizeCreatesOutput: true,
				outDir:                 jobOutputDir,
				encodeWrites:           []string{"seg_0_0.m4s", "stream_0.m3u8"},
				encode: func(_ context.Context, progress chan<- string) error {
					if progress != nil {
						progress <- "frame=1\nout_time=00:00:01.000000\nprogress=continue\n"
					}
					return cmdErr
				},
			}

			job := Job{
				Input:     filepath.Join(t.TempDir(), "in.mp4"),
				OutputDir: jobOutputDir,
				Profile:   ProfileVOD,
			}

			// A pre-existing file in the output dir must survive cleanup.
			if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
				t.Fatalf("create output dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(jobOutputDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatalf("seed output dir: %v", err)
			}

			_, err := tc.encode(context.Background(), job, exec)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var got *executor.CommandError
			if !errors.As(err, &got) {
				t.Fatalf("expected CommandError in chain, got %T (%v)", err, err)
			}
			if got.Command != "ffmpeg" || got.Stderr != cmdErr.Stderr {
				t.Fatalf("command/stderr context lost: %+v", got)
			}
			if !strings.Contains(err.Error(), cmdErr.Stderr) {
				t.Fatalf("error text lost stderr clue: %v", err)
			}

			if names := tempDirIsEmpty(t, tmpRoot); len(names) != 0 {
				t.Fatalf("temp normalized input not cleaned up: %v", names)
			}

			// Partial output must be removed, pre-existing files preserved.
			entries, err := os.ReadDir(jobOutputDir)
			if err != nil {
				t.Fatalf("read output dir: %v", err)
			}
			var outNames []string
			for _, e := range entries {
				outNames = append(outNames, e.Name())
			}
			if len(outNames) != 1 || outNames[0] != "keep.txt" {
				t.Fatalf("expected only keep.txt to remain, got %v", outNames)
			}
		})
	}
}

// TestEncodersSuccessReaches100Percent verifies the success terminal state on
// both paths: the final progress event is delivered with 100% and usage is
// returned.
func TestEncodersSuccessReaches100Percent(t *testing.T) {
	for _, tc := range encodeCases() {
		t.Run(tc.name, func(t *testing.T) {
			exec := &cancelTestExecutor{
				encode: func(_ context.Context, progress chan<- string) error {
					progress <- "frame=10\nout_time=00:00:05.000000\nprogress=continue\n"
					progress <- "frame=20\nout_time=00:00:10.000000\nprogress=end\n"
					return nil
				},
			}

			var mu sync.Mutex
			var updates []ProgressInfo
			job := Job{
				Input:     filepath.Join(t.TempDir(), "in.mp4"),
				OutputDir: filepath.Join(t.TempDir(), "out"),
				Profile:   ProfileVOD,
				ProgressHandler: func(info ProgressInfo) {
					mu.Lock()
					updates = append(updates, info)
					mu.Unlock()
				},
			}

			usage, err := tc.encode(context.Background(), job, exec)
			if err != nil {
				t.Fatalf("encode error: %v", err)
			}
			if usage == nil {
				t.Fatal("expected usage, got nil")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(updates) != 2 {
				t.Fatalf("expected 2 progress updates, got %d", len(updates))
			}
			if last := updates[len(updates)-1]; last.Percentage != 100.0 {
				t.Fatalf("expected final progress 100%%, got %v", last.Percentage)
			}
		})
	}
}
