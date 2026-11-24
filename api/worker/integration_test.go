package worker

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/types"
)

// TestLifecycleKillsRealFFmpegProcessOnCancel wires the lifecycle to the real
// encoder.FFmpeg implementation (with a fake executable standing in for ffmpeg)
// and to the in-memory job store. This exercises the production encoder
// cancellation path end to end: the lifecycle observes the cancel request,
// cancels its stage context, and the real FFmpeg process handler kills the child
// process.
func TestLifecycleKillsRealFFmpegProcessOnCancel(t *testing.T) {
	cmd := writeFakeEncoderProcess(t)
	restore := encoder.SetFFmpegCommand(cmd)
	t.Cleanup(restore)

	h := newHarness(t)
	h.markerPath = cmd + ".marker"

	l := h.lifecycle("run-1")
	l.NewEncoder = func() Encoder { return &encoder.FFmpeg{} }
	l.ProgressInterval = 5 * time.Millisecond
	l.CancelPollInterval = 5 * time.Millisecond

	result := make(chan string, 1)
	go func() {
		final, err := l.Run(nil, h.job())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		result <- final
	}()

	// Wait until the real child process is running and its stdout has been
	// parsed: only then has the production encoder path been exercised.
	waitFor(t, 15*time.Second, func() bool {
		_, _, progress := h.store.snapshot()
		return progress > 0
	}, "real encoder process to report progress")

	if ok, err := h.store.RequestJobCancelByGUID(h.job().GUID); err != nil || !ok {
		t.Fatalf("cancel request failed: ok=%v err=%v", ok, err)
	}

	select {
	case final := <-result:
		if final != types.JobCancelled {
			t.Fatalf("final status = %q, want cancelled", final)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("real ffmpeg process was not stopped by cancellation")
	}

	assertNoLeftovers(t, h.workDir, h.job().GUID)

	// On unix the fake process touches a marker file while it runs; it must not
	// be updated anymore, proving the child process was actually killed.
	if runtime.GOOS != "windows" {
		marker := h.markerPath
		before, err := os.Stat(marker)
		if err != nil {
			t.Fatalf("marker file missing while the process was running: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
		after, err := os.Stat(marker)
		if err == nil && after.ModTime().After(before.ModTime()) {
			t.Fatal("fake encoder process kept running after cancellation")
		}
	}
}

// writeFakeEncoderProcess creates an executable that behaves like a long running
// ffmpeg: it emits progress on stdout and keeps touching a marker file so the
// test can tell whether it is still alive.
func writeFakeEncoderProcess(t *testing.T) string {
	t.Helper()

	dir := tempDir(t)
	if runtime.GOOS == "windows" {
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
		"  touch \"$0.marker\"\n" +
		"  sleep 0.1\n" +
		"done\n"
	if err := ioutil.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
