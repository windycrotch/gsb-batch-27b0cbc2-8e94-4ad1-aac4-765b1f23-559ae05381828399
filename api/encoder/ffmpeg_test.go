package encoder

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeFakeFFmpeg creates a fake ffmpeg executable on disk. It is a real
// process, so the production code path (exec.CommandContext + Kill) is
// exercised without depending on a real FFmpeg installation.
func writeFakeFFmpeg(t *testing.T) string {
	t.Helper()

	dir := tempDir(t)
	if runtime.GOOS == "windows" {
		// A batch script that emits progress and then blocks. The parent kills
		// it through cmd.exe, exactly like a real ffmpeg process.
		path := filepath.Join(dir, "ffmpeg.bat")
		script := "@echo off\r\n" +
			":loop\r\n" +
			"echo frame=10\r\n" +
			"echo fps=24\r\n" +
			"echo speed=1.0x\r\n" +
			"echo progress=continue\r\n" +
			"ping -n 2 127.0.0.1 >nul\r\n" +
			"goto loop\r\n"
		if err := ioutil.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}

	path := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"while true; do\n" +
		"  echo frame=10\n" +
		"  echo fps=24\n" +
		"  echo speed=1.0x\n" +
		"  echo progress=continue\n" +
		"  sleep 0.1\n" +
		"done\n"
	if err := ioutil.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForProgress waits until the encoder observed a frame from the fake process.
func waitForProgress(t *testing.T, enc *FFmpeg) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if enc.ProgressSnapshot().Frame == 10 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake ffmpeg did not report progress")
}

func TestFFmpegRunCancelKillsProcess(t *testing.T) {
	restore := SetFFmpegCommand(writeFakeFFmpeg(t))
	defer restore()

	enc := &FFmpeg{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- enc.Run(ctx, "in.mp4", "out.mp4", `{"raw":["-y"]}`)
	}()

	waitForProgress(t, enc)

	// Cancel must stop the process without waiting for a progress poll.
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestFFmpegExplicitCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Process.Kill on the cmd.exe wrapper does not reliably terminate the
		// whole child tree in every environment; context based cancellation
		// above is the production path and is covered on all platforms.
		t.Skip("explicit Kill is covered on unix")
	}
	restore := SetFFmpegCommand(writeFakeFFmpeg(t))
	defer restore()

	enc := &FFmpeg{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- enc.Run(context.Background(), "in.mp4", "out.mp4", `{"raw":["-y"]}`)
	}()

	waitForProgress(t, enc)

	enc.Cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}
}

// TestFFmpegCancelBeforeRunIsHonoured verifies that a cancel that arrives before
// Run is called still wins: the process is never started.
func TestFFmpegCancelBeforeRunIsHonoured(t *testing.T) {
	restore := SetFFmpegCommand(writeFakeFFmpeg(t))
	defer restore()

	enc := &FFmpeg{}
	enc.Cancel()
	if err := enc.Run(context.Background(), "in.mp4", "out.mp4", `{"raw":["-y"]}`); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run error = %v, want ErrCancelled", err)
	}
}

// TestFFmpegDoneMakesLateCancelNoop verifies that a cancel arriving after the
// encoder finished does not try to signal an exited process.
func TestFFmpegDoneMakesLateCancelNoop(t *testing.T) {
	restore := SetFFmpegCommand(writeFakeFFmpeg(t))
	defer restore()

	dir := tempDir(t)
	instant := filepath.Join(dir, "ffmpeg-once")
	var script string
	if runtime.GOOS == "windows" {
		instant = filepath.Join(dir, "ffmpeg-once.bat")
		script = "@echo off\r\necho frame=1\r\necho progress=end\r\n"
	} else {
		script = "#!/bin/sh\necho frame=1\necho progress=end\n"
	}
	if err := ioutil.WriteFile(instant, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	restore2 := SetFFmpegCommand(instant)
	defer restore2()

	enc := &FFmpeg{}
	if err := enc.Run(context.Background(), "in.mp4", "out.mp4", `{"raw":["-y"]}`); err != nil {
		t.Fatalf("Run error = %v", err)
	}
	enc.Done()
	// Must not panic or kill an unrelated process.
	enc.Cancel()
}

func TestFFmpegInvalidOptionsReturnsError(t *testing.T) {
	enc := &FFmpeg{}
	if err := enc.Run(context.Background(), "in", "out", "{not json"); err == nil {
		t.Fatal("expected error for invalid options")
	}
}

func TestParseOptionsRaw(t *testing.T) {
	args, err := parseOptions("in.mp4", "out.mp4", `{"raw":["-c:v libx264","-y"]}`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range args {
		if a == "libx264" {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw options not passed through: %v", args)
	}
	if args[len(args)-1] != "out.mp4" {
		t.Fatalf("output not last: %v", args)
	}
}

func TestRunFFProbeReturnsParsedOutput(t *testing.T) {
	dir := tempDir(t)
	var path string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "ffprobe.bat")
		script := "@echo off\r\necho {\"streams\":[{\"index\":0,\"nb_frames\":\"100\"}]}\r\n"
		if err := ioutil.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	} else {
		path = filepath.Join(dir, "ffprobe")
		script := "#!/bin/sh\necho '{\"streams\":[{\"index\":0,\"nb_frames\":\"100\"}]}'\n"
		if err := ioutil.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}

	restore := SetFFprobeCommand(path)
	defer restore()

	resp, err := RunFFProbe(context.Background(), "in.mp4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Streams) != 1 || resp.Streams[0].NbFrames != "100" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestRunFFProbeHonoursCancelledContext verifies that a probe of a cancelled job
// does not start a process.
func TestRunFFProbeHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunFFProbe(ctx, "in.mp4"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// tempDir creates a temporary directory removed at the end of the test. It
// predates t.TempDir so the tests build with the repository's Go version.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := ioutil.TempDir("", "openencoder-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
