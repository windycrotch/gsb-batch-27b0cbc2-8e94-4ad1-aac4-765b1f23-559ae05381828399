package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	logger = log.New(io.Discard, "", 0)
	os.Exit(m.Run())
}

// newTestConverter builds a Converter rooted at a temp videos dir with
// fast retry timings and a stubbed ffmpeg command.
func newTestConverter(t *testing.T, runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)) (*Converter, string) {
	t.Helper()
	videosDir := t.TempDir()
	c := NewConverter(videosDir, "ffmpeg-stub")
	c.baseBackoff = time.Millisecond
	c.maxBackoff = 5 * time.Millisecond
	c.rescanInterval = 5 * time.Millisecond
	if runCmd != nil {
		c.runCmd = runCmd
	}
	return c, videosDir
}

// writeSegment creates a .ts segment inside <videosDir>/.tmp and returns
// its path plus the final output path the converter should produce.
func writeSegment(t *testing.T, videosDir, fileName, content string) (tsFile, finalFile string) {
	t.Helper()
	tmpDir := path.Join(videosDir, ".tmp")
	if err := os.MkdirAll(tmpDir, 0774); err != nil {
		t.Fatal(err)
	}
	tsFile = path.Join(tmpDir, fileName)
	if err := os.WriteFile(tsFile, []byte(content), 0664); err != nil {
		t.Fatal(err)
	}

	parts := strings.SplitN(fileName, "-", 2)
	finalFile = path.Join(videosDir, parts[0], strings.TrimSuffix(parts[1], ".ts"))
	return tsFile, finalFile
}

// succeedRunCmd stubs ffmpeg: it writes the output file (last argument).
func succeedRunCmd(content string) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out := args[len(args)-1]
		if err := os.WriteFile(out, []byte(content), 0664); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

const testSegment = "rec_2026_11_24-10_00_00_cam1.mp4.ts"

func TestConvertSuccess(t *testing.T) {
	c, videosDir := newTestConverter(t, succeedRunCmd("final-content"))
	c.Start()
	defer c.Stop()

	tsFile, finalFile := writeSegment(t, videosDir, testSegment, "segment")

	if !c.Submit(tsFile) {
		t.Fatal("submit was not accepted")
	}

	waitFor(t, "final file to appear", 5*time.Second, func() bool {
		return fileExists(finalFile)
	})

	data, err := os.ReadFile(finalFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "final-content" {
		t.Fatalf("unexpected final content: %q", data)
	}
	if fileExists(tsFile) {
		t.Fatal("source segment was not removed after successful conversion")
	}
	if fileExists(finalFile + ".part") {
		t.Fatal("temporary .part file was left behind")
	}
}

func TestConvertRetriesWithBackoffThenSucceeds(t *testing.T) {
	var calls int32
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		out := args[len(args)-1]
		if n < 3 {
			// Fail and leave partial garbage that must be cleaned up.
			os.WriteFile(out, []byte("partial"), 0664)
			return []byte("boom"), errors.New("ffmpeg failed")
		}
		return succeedRunCmd("final-content")(ctx, name, args...)
	})
	c.Start()
	defer c.Stop()

	tsFile, finalFile := writeSegment(t, videosDir, testSegment, "segment")
	c.Submit(tsFile)

	waitFor(t, "final file to appear after retries", 5*time.Second, func() bool {
		return fileExists(finalFile)
	})

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 ffmpeg attempts, got %d", got)
	}
	if fileExists(tsFile) {
		t.Fatal("source segment was not removed after successful conversion")
	}
	if fileExists(finalFile + ".part") {
		t.Fatal("partial output from failed attempts was left behind")
	}
}

func TestConvertGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("boom"), errors.New("ffmpeg failed")
	})
	c.maxAttempts = 3
	c.Start()
	defer c.Stop()

	tsFile, finalFile := writeSegment(t, videosDir, testSegment, "segment")
	c.Submit(tsFile)

	waitFor(t, "segment to be marked as failed", 5*time.Second, func() bool {
		return c.isFailed(tsFile)
	})

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 ffmpeg attempts, got %d", got)
	}
	if !fileExists(tsFile) {
		t.Fatal("source segment must be kept for manual recovery after giving up")
	}
	if fileExists(finalFile) || fileExists(finalFile+".part") {
		t.Fatal("no final or partial file may exist after giving up")
	}

	// A segment that was given up on must not be processed again, even
	// if submitted once more (e.g. by a repeated scan).
	c.Submit(tsFile)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("failed segment was processed again, attempts: %d", got)
	}
}

func TestConvertSkipsWhenFinalAlreadyExists(t *testing.T) {
	var calls int32
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("must not be called")
	})
	c.Start()
	defer c.Stop()

	tsFile, finalFile := writeSegment(t, videosDir, testSegment, "segment")

	// Simulate a previous run that already produced the final file.
	if err := os.MkdirAll(filepath.Dir(finalFile), 0774); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalFile, []byte("already-done"), 0664); err != nil {
		t.Fatal(err)
	}

	c.Submit(tsFile)

	waitFor(t, "source segment to be cleaned up", 5*time.Second, func() bool {
		return !fileExists(tsFile)
	})

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("ffmpeg ran %d times for an already converted segment", got)
	}
	data, err := os.ReadFile(finalFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "already-done" {
		t.Fatal("existing final file was overwritten")
	}
}

func TestConvertStopInterruptsInProgress(t *testing.T) {
	started := make(chan struct{}, 1)
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		started <- struct{}{}
		<-ctx.Done() // block until the converter is stopped
		return nil, ctx.Err()
	})
	c.Start()

	tsFile, finalFile := writeSegment(t, videosDir, testSegment, "segment")
	c.Submit(tsFile)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("conversion did not start")
	}

	c.Stop()

	if !fileExists(tsFile) {
		t.Fatal("source segment of an interrupted conversion must be kept")
	}
	if c.isFailed(tsFile) {
		t.Fatal("interrupted segment must not be marked as failed")
	}
	if fileExists(finalFile) || fileExists(finalFile+".part") {
		t.Fatal("interrupted conversion must not leave a final or partial file")
	}
}

func TestDuplicateSubmissionsRunOnce(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the conversion so duplicates pile up meanwhile
		return succeedRunCmd("final-content")(ctx, name, args...)
	})
	c.Start()
	defer c.Stop()

	tsFile, _ := writeSegment(t, videosDir, testSegment, "segment")

	// Same path submitted repeatedly, as recording completion and the
	// startup scan would do.
	for i := 0; i < 5; i++ {
		c.Submit(tsFile)
	}
	close(release)

	waitFor(t, "conversion to finish", 5*time.Second, func() bool {
		return !c.isInflight(tsFile) && atomic.LoadInt32(&calls) > 0
	})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 conversion for duplicate submissions, got %d", got)
	}
}

func TestScanOrphansWithFullQueue(t *testing.T) {
	c, videosDir := newTestConverter(t, succeedRunCmd("final-content"))
	// A queue that can hold only one item: the scan must not get stuck
	// and must eventually enqueue every orphan.
	c.queue = make(chan string, 1)
	c.Start()
	defer c.Stop()

	segments := []string{
		"rec_2026_11_24-10_00_00_cam1.mp4.ts",
		"rec_2026_11_24-10_05_00_cam1.mp4.ts",
		"rec_2026_11_24-10_10_00_cam1.mp4.ts",
	}
	finals := make([]string, 0, len(segments))
	for _, name := range segments {
		_, finalFile := writeSegment(t, videosDir, name, "segment")
		finals = append(finals, finalFile)
	}

	c.ScanOrphans()

	waitFor(t, "all orphaned segments to be converted", 10*time.Second, func() bool {
		for _, f := range finals {
			if !fileExists(f) {
				return false
			}
		}
		return true
	})
}

func TestScanOrphansSkipsFailedSegments(t *testing.T) {
	var calls int32
	c, videosDir := newTestConverter(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("ffmpeg failed")
	})
	c.maxAttempts = 1
	c.Start()
	defer c.Stop()

	tsFile, _ := writeSegment(t, videosDir, testSegment, "segment")
	c.Submit(tsFile)

	waitFor(t, "segment to be marked as failed", 5*time.Second, func() bool {
		return c.isFailed(tsFile)
	})
	before := atomic.LoadInt32(&calls)

	// The periodic orphan scan must not resurrect a failed segment.
	if orphans := c.findOrphans(); len(orphans) != 0 {
		t.Fatalf("failed segment reported as orphan: %v", orphans)
	}
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != before {
		t.Fatal("failed segment was converted again")
	}
	if !fileExists(tsFile) {
		t.Fatal("failed segment's source must be kept")
	}
}
