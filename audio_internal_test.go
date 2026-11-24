//go:build linux || freebsd

package oss

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFile is an audioFile that never touches the kernel, so the Audio state
// machine can be exercised without a real OSS device. It also records the
// ioctls issued against it, which lets tests assert that nothing is sent to a
// closed descriptor.
type fakeFile struct {
	mu     sync.Mutex
	fd     uintptr
	closed bool
	reqs   []uint

	// Set if an ioctl arrived while the device was already closed.
	ioctlAfterClose atomic.Int64

	writes atomic.Int64
	reads  atomic.Int64

	// When writeRelease is non-nil, Write signals writeEntered and then blocks
	// until writeRelease is closed, letting a test interleave Close/Reset.
	writeEntered chan struct{}
	writeRelease chan struct{}
}

func newFakeFile() *fakeFile {
	return &fakeFile{fd: 42}
}

func (f *fakeFile) Fd() uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.fd
}

func (f *fakeFile) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

// recordIO records an ioctl against the device.
func (f *fakeFile) recordIO(req uint) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		f.ioctlAfterClose.Add(1)
	}
	f.reqs = append(f.reqs, req)
}

func (f *fakeFile) had(req uint) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, r := range f.reqs {
		if r == req {
			return true
		}
	}

	return false
}

func (f *fakeFile) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.reqs)
}

func (f *fakeFile) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reqs = nil
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeEntered != nil {
		select {
		case f.writeEntered <- struct{}{}:
		default:
		}
		if f.writeRelease != nil {
			<-f.writeRelease
		}
	}

	f.writes.Add(1)

	if f.isClosed() {
		return 0, os.ErrClosed
	}

	return len(p), nil
}

func (f *fakeFile) Read(p []byte) (int, error) {
	f.reads.Add(1)

	if f.isClosed() {
		return 0, os.ErrClosed
	}

	return len(p), nil
}

func (f *fakeFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return os.ErrClosed
	}
	f.closed = true

	return nil
}

// fakeIO stands in for the ioctl syscall layer.
type fakeIO struct {
	file *fakeFile

	mu    sync.Mutex
	fails map[uint]error
	info  audioBufInfo

	// doFn, when set, replaces the default behaviour entirely.
	doFn func(fd uintptr, req uint, val int) (int, error)
}

func newFakeIO(file *fakeFile) *fakeIO {
	return &fakeIO{
		file:  file,
		fails: make(map[uint]error),
		info:  audioBufInfo{fragments: 2, fragstotal: 8, fragsize: 1024, bytes: 2048},
	}
}

func (f *fakeIO) do(fd uintptr, req uint, val int) (int, error) {
	f.file.recordIO(req)

	if f.doFn != nil {
		return f.doFn(fd, req, val)
	}

	f.mu.Lock()
	err, ok := f.fails[req]
	f.mu.Unlock()

	if ok {
		return 0, err
	}

	return val, nil
}

func (f *fakeIO) doInfo(fd uintptr, req uint) (audioBufInfo, error) {
	f.file.recordIO(req)

	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.fails[req]; ok {
		return audioBufInfo{}, err
	}

	return f.info, nil
}

func (f *fakeIO) fail(req uint, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fails[req] = err
}

// newTestAudio builds an Audio backed by a fake file and fake ioctl layer.
func newTestAudio(mode int) (*Audio, *fakeFile, *fakeIO) {
	file := newFakeFile()
	io := newFakeIO(file)

	a := newAudio(file, mode)
	a.ioctlFn = io.do
	a.ioctlIFn = io.doInfo

	return a, file, io
}

// errOf* adapt the various method signatures into plain error values.
func errOf(f func() (int, error)) error                { _, err := f(); return err }
func errOfIntArg(f func(int) (int, error)) error       { _, err := f(0); return err }
func errOfFormat(f func(Format) (Format, error)) error { _, err := f(AfmtS16Le); return err }
func errOfBool(f func(Cap) (bool, error)) error        { _, err := f(0); return err }
func errOfBoolFormat(f func(Format) (bool, error)) error {
	_, err := f(AfmtS16Le)
	return err
}
func errOfWrite(f func([]byte) (int, error)) error { _, err := f([]byte{0}); return err }
func errOfRead(f func([]byte) (int, error)) error  { _, err := f(make([]byte, 1)); return err }

func TestStartMatchesOpenMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          int
		wantPlaying   bool
		wantRecording bool
	}{
		{"playback", os.O_WRONLY, true, false},
		{"capture", os.O_RDONLY, false, true},
		{"duplex", os.O_RDWR, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, file, _ := newTestAudio(tt.mode)

			if err := a.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}

			if got := a.Playing(); got != tt.wantPlaying {
				t.Errorf("Playing after Start: got %v, want %v", got, tt.wantPlaying)
			}
			if got := a.Recording(); got != tt.wantRecording {
				t.Errorf("Recording after Start: got %v, want %v", got, tt.wantRecording)
			}
			if !file.had(sndctlDspSettrigger) {
				t.Error("Start did not issue DSP_SETTRIGGER")
			}
		})
	}
}

func TestStartTriggerMask(t *testing.T) {
	a, file, io := newTestAudio(os.O_RDWR)

	var got int
	io.doFn = func(_ uintptr, req uint, val int) (int, error) {
		if req == sndctlDspSettrigger {
			got = val
		}
		return val, nil
	}

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if want := pcmEnableInput | pcmEnableOutput; got != want {
		t.Errorf("trigger mask: got %#x, want %#x", got, want)
	}
	if !file.had(sndctlDspSettrigger) {
		t.Error("Start did not issue DSP_SETTRIGGER")
	}
}

func TestStartErrorLeavesStopped(t *testing.T) {
	a, _, io := newTestAudio(os.O_WRONLY)
	io.fail(sndctlDspSettrigger, errors.New("trigger failed"))

	if err := a.Start(); err == nil {
		t.Fatal("Start: expected error")
	}

	if a.Playing() || a.Recording() {
		t.Errorf("state after failed Start: playing=%v recording=%v, want false/false", a.Playing(), a.Recording())
	}
}

func TestWrongDirection(t *testing.T) {
	caps, _, _ := newTestAudio(os.O_RDONLY)
	if _, err := caps.Write([]byte{0}); !errors.Is(err, ErrNotPlayback) {
		t.Errorf("Write on capture: got %v, want ErrNotPlayback", err)
	}

	play, _, _ := newTestAudio(os.O_WRONLY)
	if _, err := play.Read(make([]byte, 1)); !errors.Is(err, ErrNotCapture) {
		t.Errorf("Read on playback: got %v, want ErrNotCapture", err)
	}

	// After Close, the closed error wins over the direction check so callers
	// get one recognizable error from every method.
	closedCaps, _, _ := newTestAudio(os.O_RDONLY)
	if err := closedCaps.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := closedCaps.Write(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Write on closed capture: got %v, want ErrClosed", err)
	}

	closedPlay, _, _ := newTestAudio(os.O_WRONLY)
	if err := closedPlay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := closedPlay.Read(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Read on closed playback: got %v, want ErrClosed", err)
	}
}

func TestResetClearsState(t *testing.T) {
	for _, mode := range []int{os.O_WRONLY, os.O_RDONLY, os.O_RDWR} {
		a, file, _ := newTestAudio(mode)

		if err := a.Start(); err != nil {
			t.Fatalf("mode %d Start: %v", mode, err)
		}

		if err := a.Reset(); err != nil {
			t.Fatalf("mode %d Reset: %v", mode, err)
		}

		if !file.had(sndctlDspReset) {
			t.Errorf("mode %d: Reset did not issue DSP_RESET", mode)
		}
		if a.Playing() {
			t.Errorf("mode %d: Playing after Reset = true, want false", mode)
		}
		if a.Recording() {
			t.Errorf("mode %d: Recording after Reset = true, want false", mode)
		}

		// A later Write/Read must not resurrect the cleared state.
		buf := make([]byte, 16)
		if mode == os.O_RDONLY {
			if _, err := a.Read(buf); err != nil {
				t.Fatalf("mode %d Read after Reset: %v", mode, err)
			}
		} else {
			if _, err := a.Write(buf); err != nil {
				t.Fatalf("mode %d Write after Reset: %v", mode, err)
			}
		}

		if a.Playing() || a.Recording() {
			t.Errorf("mode %d: state after post-Reset I/O: playing=%v recording=%v, want false/false",
				mode, a.Playing(), a.Recording())
		}
	}
}

func TestResetErrorKeepsState(t *testing.T) {
	a, _, io := newTestAudio(os.O_WRONLY)
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	io.fail(sndctlDspReset, errors.New("reset failed"))
	if err := a.Reset(); err == nil {
		t.Fatal("Reset: expected error")
	}

	// A failed Reset must not claim the device stopped.
	if !a.Playing() {
		t.Error("Playing after failed Reset = false, want true (device still running)")
	}
}

func TestAfterCloseAllMethodsReportClosed(t *testing.T) {
	a, file, _ := newTestAudio(os.O_RDWR)

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := file.requestCount()

	checks := map[string]error{
		"Caps":               errOf(a.Caps),
		"Formats":            errOf(a.Formats),
		"Nonblock":           errOf(a.Nonblock),
		"Delay":              errOf(a.Delay),
		"Post":               a.Post(),
		"Sync":               a.Sync(),
		"Reset":              a.Reset(),
		"Start":              a.Start(),
		"SetBufferSize":      a.SetBufferSize(4, 10),
		"BufferSize":         errOf(a.BufferSize),
		"RecordedBufferSize": errOf(a.RecordedBufferSize),
		"UnplayedBufferSize": errOf(a.UnplayedBufferSize),
		"Format":             errOfFormat(a.Format),
		"Samplerate":         errOfIntArg(a.Samplerate),
		"Channels":           errOfIntArg(a.Channels),
		"Write":              errOfWrite(a.Write),
		"Read":               errOfRead(a.Read),
		"IsCap":              errOfBool(a.IsCap),
		"IsFormat":           errOfBoolFormat(a.IsFormat),
	}

	for name, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: got %v, want ErrClosed", name, err)
		}
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("%s after Close: error does not match os.ErrClosed", name)
		}
	}

	if n := file.ioctlAfterClose.Load(); n != 0 {
		t.Errorf("ioctl issued after Close: %d", n)
	}
	if got := file.requestCount(); got != before {
		t.Errorf("descriptor touched after Close: ioctl count %d -> %d", before, got)
	}

	if a.Playing() || a.Recording() {
		t.Error("state after Close: want false/false")
	}

	// Closing twice must not panic and must be recognizable.
	if err := a.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: got %v, want ErrClosed", err)
	}
}

func TestUnplayedBufferSizeOnCapture(t *testing.T) {
	a, file, io := newTestAudio(os.O_RDONLY)

	bufSize, err := a.BufferSize()
	if err != nil {
		t.Fatalf("BufferSize on capture: %v", err)
	}
	if want := int(io.info.fragstotal * io.info.fragsize); bufSize != want {
		t.Errorf("BufferSize on capture: got %d, want %d", bufSize, want)
	}
	if !file.had(sndctlDspGetispace) {
		t.Error("BufferSize on capture did not query DSP_GETISPACE")
	}
	if file.had(sndctlDspGetospace) {
		t.Error("BufferSize on capture used playback space ioctl")
	}

	if _, err := a.RecordedBufferSize(); err != nil {
		t.Errorf("RecordedBufferSize on capture: %v", err)
	}

	file.resetRequests()

	if _, err := a.UnplayedBufferSize(); !errors.Is(err, ErrNotPlayback) {
		t.Errorf("UnplayedBufferSize on capture: got %v, want ErrNotPlayback", err)
	}
	if file.had(sndctlDspGetospace) {
		t.Error("UnplayedBufferSize on capture issued a playback ioctl")
	}
}

func TestUnplayedBufferSizeOnPlayback(t *testing.T) {
	a, _, io := newTestAudio(os.O_WRONLY)

	got, err := a.UnplayedBufferSize()
	if err != nil {
		t.Fatalf("UnplayedBufferSize on playback: %v", err)
	}
	want := int(io.info.fragstotal*io.info.fragsize - io.info.bytes)
	if got != want {
		t.Errorf("UnplayedBufferSize: got %d, want %d", got, want)
	}
}

func TestConcurrentWriteAndClose(t *testing.T) {
	a, file, _ := newTestAudio(os.O_WRONLY)

	file.writeEntered = make(chan struct{}, 1)
	file.writeRelease = make(chan struct{})

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !a.Playing() {
		t.Fatal("Playing after Start = false")
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := a.Write(make([]byte, 128))
		writeErr <- err
	}()

	select {
	case <-file.writeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not start")
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.Playing() {
		t.Error("Playing after Close = true, want false")
	}

	close(file.writeRelease)

	if err := <-writeErr; !errors.Is(err, ErrClosed) {
		t.Errorf("concurrent Write: got %v, want ErrClosed", err)
	}
	if a.Playing() {
		t.Error("Playing resurrected by concurrent Write, want false")
	}
}

func TestConcurrentWriteAndReset(t *testing.T) {
	a, file, _ := newTestAudio(os.O_WRONLY)

	file.writeEntered = make(chan struct{}, 1)
	file.writeRelease = make(chan struct{})

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := a.Write(make([]byte, 128))
		writeErr <- err
	}()

	select {
	case <-file.writeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not start")
	}

	if err := a.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if a.Playing() {
		t.Error("Playing after Reset = true, want false")
	}

	close(file.writeRelease)

	if err := <-writeErr; err != nil {
		t.Fatalf("concurrent Write: %v", err)
	}
	if a.Playing() {
		t.Error("Reset state overwritten by concurrent Write, want false")
	}
}

func TestNormalPlaybackWorkflow(t *testing.T) {
	a, file, io := newTestAudio(os.O_WRONLY)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !a.Playing() || a.Recording() {
		t.Fatalf("after Start: playing=%v recording=%v, want true/false", a.Playing(), a.Recording())
	}

	n, err := a.Write(make([]byte, 64))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 64 {
		t.Errorf("Write: got %d bytes, want 64", n)
	}
	if !a.Playing() {
		t.Error("Playing after successful Write = false, want true")
	}
	if file.writes.Load() != 1 {
		t.Errorf("underlying writes: got %d, want 1", file.writes.Load())
	}

	if got, err := a.BufferSize(); err != nil {
		t.Errorf("BufferSize: %v", err)
	} else if want := int(io.info.fragstotal * io.info.fragsize); got != want {
		t.Errorf("BufferSize: got %d, want %d", got, want)
	}

	if got, err := a.UnplayedBufferSize(); err != nil {
		t.Errorf("UnplayedBufferSize: %v", err)
	} else if want := int(io.info.fragstotal*io.info.fragsize - io.info.bytes); got != want {
		t.Errorf("UnplayedBufferSize: got %d, want %d", got, want)
	}

	// A read on a playback-only device is still rejected.
	if _, err := a.Read(make([]byte, 8)); !errors.Is(err, ErrNotCapture) {
		t.Errorf("Read on playback: got %v, want ErrNotCapture", err)
	}

	if err := a.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if a.Playing() || a.Recording() {
		t.Errorf("after Reset: playing=%v recording=%v, want false/false", a.Playing(), a.Recording())
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNormalCaptureWorkflow(t *testing.T) {
	a, file, _ := newTestAudio(os.O_RDONLY)

	buf := make([]byte, 32)
	n, err := a.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(buf) {
		t.Errorf("Read: got %d bytes, want %d", n, len(buf))
	}
	if file.reads.Load() != 1 {
		t.Errorf("underlying reads: got %d, want 1", file.reads.Load())
	}

	if _, err := a.Write(buf); !errors.Is(err, ErrNotPlayback) {
		t.Errorf("Write on capture: got %v, want ErrNotPlayback", err)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestConcurrentStress hammers Write/Reset/Close from several goroutines and
// checks that the object never panics and always converges to a consistent
// state (a closed device is never reported as playing).
func TestConcurrentStress(t *testing.T) {
	a, _, _ := newTestAudio(os.O_WRONLY)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a.Write(make([]byte, 16))
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			a.Reset()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			a.Playing()
			a.Recording()
		}
	}()

	time.Sleep(5 * time.Millisecond)

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wg.Wait()

	if a.Playing() || a.Recording() {
		t.Errorf("final state: playing=%v recording=%v, want false/false", a.Playing(), a.Recording())
	}
	if _, err := a.Write(make([]byte, 1)); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close: got %v, want ErrClosed", err)
	}
}

func TestConcurrentCloseWhileIoctl(t *testing.T) {
	a, file, io := newTestAudio(os.O_WRONLY)

	// Slow the ioctl path down so Close and the ioctl overlap.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	io.doFn = func(fd uintptr, req uint, val int) (int, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return val, nil
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- a.Start()
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ioctl did not start")
	}

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- a.Close()
	}()

	// Give Close a chance to contend for the lock, then let the ioctl finish.
	time.Sleep(20 * time.Millisecond)
	close(release)

	if err := <-startErr; err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := <-closeErr; err != nil {
		t.Errorf("Close: %v", err)
	}

	if a.Playing() || a.Recording() {
		t.Error("state after Close: want false/false")
	}
	if _, err := a.Caps(); !errors.Is(err, ErrClosed) {
		t.Errorf("Caps after Close: got %v, want ErrClosed", err)
	}
	if n := file.ioctlAfterClose.Load(); n != 0 {
		t.Errorf("ioctl issued after Close: %d", n)
	}
}
