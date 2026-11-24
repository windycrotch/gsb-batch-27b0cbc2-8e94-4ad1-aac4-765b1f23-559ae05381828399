package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// ErrSessionClosed is returned when a frame is requested from a session that
// has already been closed. Callers should treat it as a terminal condition and
// must not schedule further reads.
var ErrSessionClosed = errors.New("video session is closed")

// FrameSource is a stream of fixed-size RGB24 frames.
//
// It is implemented by Session for real FFmpeg capture and by test doubles in
// higher layers, so that the application lifecycle can be tested without a
// camera or FFmpeg installed.
type FrameSource interface {
	// ReadFrame fills dest with exactly one frame of width*height*3 bytes.
	ReadFrame(dest []byte) error
	// Close releases the source. It must unblock any in-flight ReadFrame call,
	// be safe to call concurrently with ReadFrame, and be idempotent.
	Close() error
}

// Device identifies a camera as exposed by the host platform.

type Device struct {
	ID   string
	Name string
}

func (d Device) String() string {
	if d.Name == "" || d.Name == d.ID {
		return d.ID
	}

	return fmt.Sprintf("%s (%s)", d.Name, d.ID)
}

// commandContext is a seam that lets tests replace the ffmpeg executable with
// a harmless helper process.
var commandContext = exec.CommandContext //nolint:gochecknoglobals // test seam

// Session manages the lifecycle of the background FFmpeg capture process.
type Session struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *stderrBuffer
	cancel context.CancelFunc
	width  int
	height int
	closed bool
}

type stderrBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func newStderrBuffer(limit int) *stderrBuffer {
	return &stderrBuffer{limit: limit}
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}

	b.data = append(b.data, p...)
	if overflow := len(b.data) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:b.limit]
	}

	return len(p), nil
}

func (b *stderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return strings.TrimSpace(string(b.data))
}

func newSession(width, height int, args []string) (*Session, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// #nosec G204 -- Safe. The args array is constructed entirely inside internal platform-specific files using static system parameters.
	cmd := commandContext(ctx, "ffmpeg", args...)
	stderr := newStderrBuffer(8192)
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()

	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create ffmpeg stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdout.Close()
		return nil, fmt.Errorf("failed to start ffmpeg subprocess: %w", err)
	}

	return &Session{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
		cancel: cancel,
		width:  width,
		height: height,
	}, nil
}

// Width reports the width in pixels of every frame produced by the session.
func (s *Session) Width() int { return s.width }

// Height reports the height in pixels of every frame produced by the session.
func (s *Session) Height() int { return s.height }

// FrameSize reports the number of RGB24 bytes in a single frame.
func (s *Session) FrameSize() int { return s.width * s.height * 3 }

// ReadFrame reads a single frame of video data into the buffer.
//
// The frame always has FrameSize() bytes; dest must be at least that large.
// Once Close has been called, ReadFrame returns ErrSessionClosed.
func (s *Session) ReadFrame(dest []byte) error {
	expectedSize := s.width * s.height * 3
	if expectedSize <= 0 {
		return fmt.Errorf("invalid session frame size: %dx%d", s.width, s.height)
	}
	if len(dest) < expectedSize {
		return fmt.Errorf("destination buffer too small: got %d bytes, need at least %d", len(dest), expectedSize)
	}

	// Snapshot the reader under the lock: Close may replace it concurrently.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	stdout := s.stdout
	s.mu.Unlock()

	if stdout == nil {
		return ErrSessionClosed
	}

	_, err := io.ReadFull(stdout, dest[:expectedSize])
	if err == nil {
		return nil
	}

	// A concurrent Close is not an error worth reporting as a capture failure.
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrSessionClosed
	}

	if stderr := s.stderr.String(); stderr != "" {
		return fmt.Errorf("failed to read ffmpeg frame: %w: %s", err, stderr)
	}

	return err
}

// Close gracefully cancels the execution and reaps the child process.
//
// Closing the stdout pipe first unblocks any goroutine blocked in ReadFrame;
// the context cancellation then terminates ffmpeg and Wait reaps it, so no
// zombie process is left behind. Close is idempotent and safe to call
// concurrently with ReadFrame and with itself.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stdout, cmd, cancel := s.stdout, s.cmd, s.cancel
	s.stdout = nil
	s.mu.Unlock()

	if stdout != nil {
		_ = stdout.Close()
	}
	if cancel != nil {
		cancel()
	}
	if cmd != nil {
		// The process is expected to be killed by the canceled context; the
		// resulting exit error is not interesting to callers. Wait still has
		// to run so the child is reaped instead of lingering as a zombie.
		_ = cmd.Wait()
	}

	return nil
}

// Closed reports whether Close has been called.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

var _ FrameSource = (*Session)(nil)
