package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests below never touch a real camera or a real ffmpeg binary. Instead
// commandContext is pointed at this test binary, which then acts as a stand-in
// ffmpeg process writing raw RGB24 frames to stdout.

func TestVideoHelperProcess(t *testing.T) {
	mode := os.Getenv("TERMICAM_TEST_HELPER")
	if mode == "" {
		t.Skip("helper process only")
	}

	switch mode {
	case "frames":
		count, _ := strconv.Atoi(os.Getenv("TERMICAM_TEST_FRAMES"))
		size, _ := strconv.Atoi(os.Getenv("TERMICAM_TEST_FRAME_SIZE"))
		frame := bytes.Repeat([]byte{0xAB}, size)
		for i := 0; i < count; i++ {
			if _, err := os.Stdout.Write(frame); err != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
	case "block":
		// Write nothing and block forever; the parent's Close must terminate us.
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

// useHelperProcess replaces the ffmpeg command with this test binary in helper
// mode and restores the original on cleanup.
func useHelperProcess(t *testing.T, env ...string) {
	t.Helper()

	original := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// #nosec G204 -- Test-only invocation of the test binary itself.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestVideoHelperProcess", "--")
		cmd.Env = append(os.Environ(), env...)
		return cmd
	}
	t.Cleanup(func() { commandContext = original })
}

func newTestSession(t *testing.T, width, height int) *Session {
	t.Helper()

	session, err := newSession(width, height, []string{"unused"})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	return session
}

func TestSessionReadsFramesOfExpectedSize(t *testing.T) {
	useHelperProcess(t,
		"TERMICAM_TEST_HELPER=frames",
		"TERMICAM_TEST_FRAMES=2",
		"TERMICAM_TEST_FRAME_SIZE=24", // 2x4x3
	)

	session := newTestSession(t, 2, 4)
	defer func() { _ = session.Close() }()

	if got, want := session.FrameSize(), 24; got != want {
		t.Fatalf("FrameSize() = %d, want %d", got, want)
	}

	buf := make([]byte, session.FrameSize())
	for i := 0; i < 2; i++ {
		if err := session.ReadFrame(buf); err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if buf[0] != 0xAB {
			t.Fatalf("frame %d not filled: first byte %#x", i, buf[0])
		}
	}

	// The helper exits after two frames; the read must surface an error.
	if err := session.ReadFrame(buf); err == nil {
		t.Fatal("expected an error after the helper exited")
	}
}

func TestSessionReadFrameRejectsSmallBuffer(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=block")

	session := newTestSession(t, 4, 4)
	defer func() { _ = session.Close() }()

	err := session.ReadFrame(make([]byte, 10))
	if err == nil || !strings.Contains(err.Error(), "buffer too small") {
		t.Fatalf("expected buffer-too-small error, got %v", err)
	}
}

func TestSessionCloseUnblocksPendingRead(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=block")

	session := newTestSession(t, 4, 4)

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		done <- result{session.ReadFrame(make([]byte, session.FrameSize()))}
	}()

	// Give the reader a moment to block inside ReadFull.
	time.Sleep(100 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: pending read was not unblocked / process not reaped")
	}

	select {
	case res := <-done:
		if !errors.Is(res.err, ErrSessionClosed) {
			t.Fatalf("pending ReadFrame error = %v, want ErrSessionClosed", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReadFrame did not return after Close")
	}

	if !session.Closed() {
		t.Fatal("session should report closed")
	}
}

func TestSessionCloseReapsProcessAndIsIdempotent(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=block")

	session := newTestSession(t, 4, 4)

	if err := session.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if session.cmd.ProcessState == nil {
		t.Fatal("child process was not waited on (ProcessState is nil)")
	}
	if !session.cmd.ProcessState.Exited() && session.cmd.ProcessState.String() == "" {
		t.Fatalf("unexpected process state: %v", session.cmd.ProcessState)
	}

	// A second Close must not panic, must not wait again and must stay nil-safe.
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := session.ReadFrame(make([]byte, session.FrameSize())); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("ReadFrame after Close = %v, want ErrSessionClosed", err)
	}
}

func TestSessionCloseConcurrent(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=block")

	session := newTestSession(t, 4, 4)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = session.Close()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Close calls did not finish")
	}

	if session.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
}

func TestSessionInvalidFrameSize(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=block")

	session := newTestSession(t, 0, 0)
	defer func() { _ = session.Close() }()

	err := session.ReadFrame(nil)
	if err == nil || !strings.Contains(err.Error(), "invalid session frame size") {
		t.Fatalf("expected invalid frame size error, got %v", err)
	}
}

func TestFrameSourceInterface(t *testing.T) {
	useHelperProcess(t, "TERMICAM_TEST_HELPER=frames", "TERMICAM_TEST_FRAMES=1", "TERMICAM_TEST_FRAME_SIZE=12")

	var source FrameSource = newTestSession(t, 2, 2)
	defer func() { _ = source.Close() }()

	if err := source.ReadFrame(make([]byte, 12)); err != nil {
		t.Fatalf("ReadFrame through interface: %v", err)
	}
}

// Ensure the exported error is stable enough to be matched by callers.
func TestErrSessionClosedMessage(t *testing.T) {
	if errors.Is(ErrSessionClosed, nil) {
		t.Fatal("ErrSessionClosed must not be nil")
	}
	if !strings.Contains(fmt.Sprint(ErrSessionClosed), "closed") {
		t.Fatalf("unexpected ErrSessionClosed message: %q", ErrSessionClosed)
	}
}
