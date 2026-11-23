package spireav

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Progress struct {
	Frame      int64
	FPS        float64
	Bitrate    int64
	TotalSize  int64
	OutTime    string
	DupFrames  int64
	DropFrames int64
	Speed      float64
	Done       bool
	Started    time.Time
	Elapsed    time.Duration
}

type Runner interface {
	Run(ctx context.Context) error
	CommandString() (string, error)
}

type Option func(*implRunner)

func WithProgressCallback(callback func(Progress)) Option {
	return func(r *implRunner) {
		r.progressCallback = callback
	}
}

func WithSysProcAttr(attr *syscall.SysProcAttr) Option {
	return func(r *implRunner) {
		r.sysProcAttr = attr
	}
}

func NewRunner(runnable Runnable, opts ...Option) Runner {
	runner := &implRunner{
		runnable: runnable,
	}
	for _, opt := range opts {
		opt(runner)
	}
	return runner
}

type implRunner struct {
	runnable         Runnable
	progressCallback func(Progress)
	sysProcAttr      *syscall.SysProcAttr
}

// CommandString is a utility function that gets the FFmpeg command string that is run by this process
func (p *implRunner) CommandString() (string, error) {
	args, err := p.runnable.RunnableArgs()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s %s\n",
		FfmpegPath,
		strings.Join(args, " "),
	), nil
}

// Run executes the transcoding process and blocks the calling thread until the process is completed or failed.
//
// If a progress callback is provided, it is invoked once per progress update emitted by FFmpeg, in
// the exact order FFmpeg produced them. On success, the final invocation carries Done=true. If the
// process fails or the context is canceled, no Done=true callback is ever delivered, and no callback
// is invoked after Run returns.
//
// If ctx is canceled, Run kills the process, stops delivering progress updates, and returns the
// context's error (context.Canceled or context.DeadlineExceeded), which is distinguishable from an
// FFmpeg failure via errors.Is. An FFmpeg failure is returned as *FfmpegError.
func (p *implRunner) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Generate the FFmpeg arguments
	args, err := p.runnable.RunnableArgs()
	if err != nil {
		return fmt.Errorf("generating ffmpeg args: %w", err)
	}

	// Create the command
	cmd := exec.CommandContext(ctx, FfmpegPath, args...)
	cmd.SysProcAttr = p.sysProcAttr

	// Stderr will contain error messages ONLY
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("acquiring stderr pipe: %w", err)
	}
	defer stderr.Close()

	// Stdout will contain a streaming progress report
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("acquiring stdout pipe: %w", err)
	}
	defer stdout.Close()

	// Capture and buffer stderr
	var stderrBuf []byte

	// The last parsed progress state, retained so the terminal Done=true callback can be
	// delivered by Run itself once the process is known to have succeeded.
	var lastProgress Progress

	// Parse the output into progress reports on the channel
	// Also make sure we collect all of stderr before returning any error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if p.progressCallback != nil {
			lastProgress = p.reportFFmpegProgress(ctx, stdout)
		} else {
			io.Copy(io.Discard, stdout)
		}
	}()
	go func() {
		defer wg.Done()
		stderrBuf, _ = io.ReadAll(stderr)
	}()

	// Start running the command in the background
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg process: %w", err)
	}

	// Wait for the process to end
	waitErr := cmd.Wait()

	// Wait for the output parsers to finish. This guarantees no progress callback is
	// still in flight (or will ever be invoked) after Run returns.
	wg.Wait()

	// Cancellation takes precedence over the process's own exit status: if the context
	// is done, the run was interrupted, so report the cancellation and never emit a
	// completion callback.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// If the process errored, return the error
	if waitErr != nil {
		return &FfmpegError{
			ExitCode: cmd.ProcessState.ExitCode(),
			Logs:     strings.Split(strings.TrimSpace(string(stderrBuf)), "\n"),
			child:    waitErr,
		}
	}

	// The process succeeded. Deliver the terminal Done=true callback as the very last
	// progress update, synchronously, so it is guaranteed to be ordered after every
	// other update and to never occur for a failed or canceled run.
	if p.progressCallback != nil {
		lastProgress.Done = true
		lastProgress.Elapsed = time.Since(lastProgress.Started)
		p.progressCallback(lastProgress)
	}
	return nil
}

// reportFFmpegProgress parses FFmpeg's machine-readable progress stream, invoking the
// progress callback once per update block, in stream order. Intermediate ("continue")
// updates are delivered as they arrive, unless the context has been canceled. The
// terminal ("end") update is withheld and its state returned, so the caller can deliver
// it only after confirming the process exited successfully.
func (p *implRunner) reportFFmpegProgress(ctx context.Context, processOutput io.Reader) Progress {
	// Initialize the progress to empty
	var progress Progress
	progress.Started = time.Now()

	// Create a scanner for the standard output
	scanner := bufio.NewScanner(processOutput)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		// Read a line from the input
		line := strings.TrimSpace(scanner.Text())
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		// Get the key and value
		key, value := parts[0], parts[1]
		switch key {
		case "frame":
			progress.Frame, _ = strconv.ParseInt(value, 10, 64)
		case "fps":
			progress.FPS, _ = strconv.ParseFloat(value, 64)
		case "bitrate":
			kbps, _ := strconv.ParseFloat(strings.TrimSuffix(value, "kbits/s"), 64)
			progress.Bitrate = int64(kbps * 1000)
		case "total_size":
			progress.TotalSize, _ = strconv.ParseInt(value, 10, 64)
		case "out_time":
			progress.OutTime = value
		case "dup_frames":
			progress.DupFrames, _ = strconv.ParseInt(value, 10, 64)
		case "drop_frames":
			progress.DropFrames, _ = strconv.ParseInt(value, 10, 64)
		case "speed":
			progress.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64)
		case "progress":
			progress.Done = (value == "end")
		}

		// If the key is "progress", a complete update block has been received
		if key != "progress" {
			continue
		}

		// Withhold the terminal update: Run delivers it only on success
		if progress.Done {
			continue
		}

		// Once the context is canceled, no further updates may be delivered. Keep
		// draining the stream so the process is never blocked on a full pipe.
		if ctx.Err() != nil {
			continue
		}

		progress.Elapsed = time.Since(progress.Started)
		p.progressCallback(progress)
	}
	return progress
}
