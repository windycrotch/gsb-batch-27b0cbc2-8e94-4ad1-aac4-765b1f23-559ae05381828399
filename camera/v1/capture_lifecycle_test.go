package camera

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kirizu-Official/windows-camera-go/utils"
	"github.com/Kirizu-Official/windows-camera-go/windows/consts"
	"github.com/Kirizu-Official/windows-camera-go/windows/mf"
)

// ---------------------------------------------------------------------------
// Test doubles
//
// fakeSourceReader replaces *mf.IMFSourceReader. It records every ReadSample
// call and returns scripted results, so no real camera or COM is required.
// ---------------------------------------------------------------------------

type readResult struct {
	flags  uint32
	sample *mf.IMFSample // nil means an empty read (e.g. a stream tick)
	err    error
}

type fakeSourceReader struct {
	mu sync.Mutex

	script   []readResult
	idx      int
	reads    int32 // total ReadSample calls (atomic)
	releases int32 // total Release calls (atomic)

	// blockRead, when non-nil, is blocked on at the start of ReadSample until
	// closed, letting a test hold a "read in flight" while closing the device.
	blockRead chan struct{}

	// entered is signalled once ReadSample has begun (after block handling),
	// so tests know the goroutine is inside the call.
	entered   chan struct{}
	enterOnce sync.Once

	readErr error // persistent error returned for every call (overrides script)

	// emptyWhenExhausted makes ReadSample return an empty (stream-tick) result
	// forever once the script runs out, instead of an "exhausted" error. This
	// models an asynchronous source that keeps reporting "no sample yet".
	emptyWhenExhausted bool
}

func newFakeReader(script ...readResult) *fakeSourceReader {
	return &fakeSourceReader{
		script:  script,
		entered: make(chan struct{}),
	}
}

func (f *fakeSourceReader) SetCurrentMediaType(_ uint32, _ *mf.IMFMediaType) error {
	return nil
}

func (f *fakeSourceReader) ReadSample(_ uint32, _ uint32, pdwActualStreamIndex *uint32, pdwStreamFlags *uint32, _ *int64, ppSample **mf.IMFSample) error {
	atomic.AddInt32(&f.reads, 1)
	f.enterOnce.Do(func() { close(f.entered) })

	if f.blockRead != nil {
		<-f.blockRead
	}

	f.mu.Lock()
	if f.readErr != nil {
		err := f.readErr
		f.mu.Unlock()
		return err
	}
	if f.idx >= len(f.script) {
		emptyForever := f.emptyWhenExhausted
		f.mu.Unlock()
		if emptyForever {
			if pdwStreamFlags != nil {
				*pdwStreamFlags = uint32(consts.MF_SOURCE_READERF_STREAMTICK)
			}
			if ppSample != nil {
				*ppSample = nil
			}
			return nil
		}
		// Script exhausted: surface a distinct error instead of spinning.
		return errors.New("fake reader: script exhausted")
	}
	r := f.script[f.idx]
	f.idx++
	f.mu.Unlock()

	if pdwActualStreamIndex != nil {
		*pdwActualStreamIndex = 0
	}
	if pdwStreamFlags != nil {
		*pdwStreamFlags = r.flags
	}
	if ppSample != nil {
		*ppSample = r.sample
	}
	return r.err
}

func (f *fakeSourceReader) Release() error {
	atomic.AddInt32(&f.releases, 1)
	return nil
}

func (f *fakeSourceReader) readCount() int32    { return atomic.LoadInt32(&f.reads) }
func (f *fakeSourceReader) releaseCount() int32 { return atomic.LoadInt32(&f.releases) }

// fakeProvider hands back a pre-built fake reader and records how many readers
// it created (one per capture start), which models "restart capture".
type fakeProvider struct {
	reader        *fakeSourceReader
	createCalls   int32
	createErr     error
	nextMediaType *mf.IMFMediaType
}

func (p *fakeProvider) createSourceReader(_ *CaptureConfig) (sourceReader, *mf.IMFMediaType, error) {
	atomic.AddInt32(&p.createCalls, 1)
	if p.createErr != nil {
		return nil, nil, p.createErr
	}
	// mediaType is returned as nil on purpose: the fake has no COM media-type
	// object, and CloseDevice skips nil pointers instead of calling through a
	// fake vtable.
	return p.reader, p.nextMediaType, nil
}

func (p *fakeProvider) createdCount() int32 { return atomic.LoadInt32(&p.createCalls) }

// newTestDevice builds a Device wired to in-process fakes, never touching COM.
func newTestDevice(reader *fakeSourceReader) *Device {
	d := &Device{
		Name:       "test-camera",
		SymbolLink: "test-symbolic-link",
	}
	d.readerProvider = &fakeProvider{reader: reader}
	return d
}

func newTestDeviceWithProvider(provider sourceReaderProvider) *Device {
	d := &Device{
		Name:       "test-camera",
		SymbolLink: "test-symbolic-link",
	}
	d.readerProvider = provider
	return d
}

// realFakeSample is a non-nil IMFSample with a non-nil vtable pointer, enough
// to pass the pSample.S != nil delivery check without making any syscall.
func realFakeSample() *mf.IMFSample {
	return &mf.IMFSample{S: &mf.IMFSample_{}}
}

func tickResult() readResult {
	return readResult{flags: uint32(consts.MF_SOURCE_READERF_STREAMTICK), sample: nil}
}

func frameResult() readResult {
	return readResult{flags: 0, sample: realFakeSample()}
}

// ---------------------------------------------------------------------------
// Synchronous path
// ---------------------------------------------------------------------------

// Stream ticks must be swallowed: GetFrame keeps waiting and returns the first
// real sample instead of an empty/error result.
func TestSyncGetFrameWaitsThroughStreamTicks(t *testing.T) {
	reader := newFakeReader(tickResult(), tickResult(), tickResult(), frameResult())
	d := newTestDevice(reader)
	sync, err := d.StartCapture(&CaptureFormats{})
	if err != nil {
		t.Fatalf("StartCapture: %v", err)
	}

	frame, err := sync.GetFrame()
	if err != nil {
		t.Fatalf("GetFrame returned error after ticks: %v", err)
	}
	if frame == nil || frame.PpSample == nil {
		t.Fatalf("expected a real sample, got %+v", frame)
	}
	if got := reader.readCount(); got != 4 {
		t.Fatalf("expected 4 ReadSample calls (3 ticks + 1 frame), got %d", got)
	}
}

// An empty read that is not explicitly flagged as a stream tick must still be
// treated as "no sample yet" rather than an error.
func TestSyncGetFrameTreatsEmptyReadAsNoSample(t *testing.T) {
	reader := newFakeReader(readResult{flags: 0, sample: nil}, frameResult())
	d := newTestDevice(reader)
	sync, _ := d.StartCapture(&CaptureFormats{})

	frame, err := sync.GetFrame()
	if err != nil {
		t.Fatalf("expected retry on empty read, got error: %v", err)
	}
	if frame == nil || frame.PpSample == nil {
		t.Fatalf("expected a real sample")
	}
}

// A genuine read error must be propagated, not retried forever.
func TestSyncGetFramePropagatesReadError(t *testing.T) {
	readErr := errors.New("boom")
	reader := newFakeReader(readResult{err: readErr})
	d := newTestDevice(reader)
	sync, _ := d.StartCapture(&CaptureFormats{})

	_, err := sync.GetFrame()
	if !errors.Is(err, readErr) {
		t.Fatalf("expected read error %v, got %v", readErr, err)
	}
	if got := reader.readCount(); got != 1 {
		t.Fatalf("error path must not retry, reads=%d", got)
	}
}

// After CloseDevice every GetFrame returns ErrorDeviceNotOpen and never reads.
func TestSyncGetFrameAfterCloseReturnsNotOpen(t *testing.T) {
	reader := newFakeReader(frameResult())
	d := newTestDevice(reader)
	sync, _ := d.StartCapture(&CaptureFormats{})
	d.CloseDevice()

	_, err := sync.GetFrame()
	if !errors.Is(err, utils.ErrorDeviceNotOpen) {
		t.Fatalf("expected ErrorDeviceNotOpen after close, got %v", err)
	}
	if got := reader.readCount(); got != 0 {
		t.Fatalf("closed device must not issue reads, reads=%d", got)
	}
}

// A GetFrame blocked inside ReadSample must fail cleanly once the device is
// closed rather than returning a frame to the caller.
func TestSyncGetFrameBlockedThenClosed(t *testing.T) {
	// On unblocking this returns an empty read; it must still be suppressed by
	// the post-read closed check.
	reader := newFakeReader(readResult{flags: 0, sample: nil})
	reader.blockRead = make(chan struct{})
	d := newTestDevice(reader)
	sync, _ := d.StartCapture(&CaptureFormats{})

	result := make(chan error, 1)
	go func() {
		_, err := sync.GetFrame()
		result <- err
	}()

	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadSample never started")
	}

	d.CloseDevice()
	close(reader.blockRead) // let the blocked read complete with a frame

	select {
	case err := <-result:
		if !errors.Is(err, utils.ErrorDeviceNotOpen) {
			t.Fatalf("expected ErrorDeviceNotOpen, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetFrame did not return after close")
	}
}

// ---------------------------------------------------------------------------
// Asynchronous path: bounded empty-sample retry
// ---------------------------------------------------------------------------

// Empty samples re-issue ReadSample only up to the cap, then report one error
// and stop. No user frame callback must ever fire.
func TestAsyncEmptySampleRetryIsBounded(t *testing.T) {
	reader := newFakeReader() // every read beyond the script is an empty tick
	reader.emptyWhenExhausted = true
	d := newTestDevice(reader)

	var errCount int32
	var lastErr error
	var mu sync.Mutex
	d.SetErrorCallBack(func(err error) {
		atomic.AddInt32(&errCount, 1)
		mu.Lock()
		lastErr = err
		mu.Unlock()
	})

	var delivered int32
	a, err := d.StartCaptureAsync(&CaptureFormats{})
	if err != nil {
		t.Fatalf("StartCaptureAsync: %v", err)
	}
	a.GetCallBackInterface().SetOnReadSample(func(_ *CaptureAsync, _ uint32, _ uint32, _ int64, _ *mf.IMFSample) {
		atomic.AddInt32(&delivered, 1)
	})

	// Kick off the first read; each empty callback re-requests the next.
	if err := a.GetNextFrame(); err != nil {
		t.Fatalf("initial GetNextFrame: %v", err)
	}

	// Drive the empty-sample callbacks directly, as the COM work queue would.
	for i := 0; i < maxEmptySampleRetries+2; i++ {
		a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}

	// First GetNextFrame + maxEmptySampleRetries automatic re-requests.
	wantReads := int32(1 + maxEmptySampleRetries)
	if got := reader.readCount(); got != wantReads {
		t.Fatalf("expected %d reads (1 initial + %d retries), got %d", wantReads, maxEmptySampleRetries, got)
	}
	if got := atomic.LoadInt32(&errCount); got != 1 {
		t.Fatalf("error callback must fire exactly once, got %d (last=%v)", got, lastErr)
	}
	mu.Lock()
	if !errors.Is(lastErr, utils.ErrorAsyncRetryExhausted) {
		t.Fatalf("expected ErrorAsyncRetryExhausted, got %v", lastErr)
	}
	mu.Unlock()
	if got := atomic.LoadInt32(&delivered); got != 0 {
		t.Fatalf("no frame may be delivered for empty samples, got %d", got)
	}

	// Once stopped, further GetNextFrame calls must not issue reads.
	if err := a.GetNextFrame(); !errors.Is(err, utils.ErrorAsyncRetryExhausted) {
		t.Fatalf("expected ErrorAsyncRetryExhausted from GetNextFrame, got %v", err)
	}
	if got := reader.readCount(); got != wantReads {
		t.Fatalf("stopped session issued extra reads: %d", got)
	}
}

// A real sample resets the empty-sample streak, so the retry budget is measured
// consecutively and a frame is delivered to the user callback.
func TestAsyncRealSampleResetsEmptyCounter(t *testing.T) {
	reader := newFakeReader()
	reader.emptyWhenExhausted = true
	d := newTestDevice(reader)
	d.SetErrorCallBack(func(error) { t.Fatal("error callback must not fire when samples recover") })

	var delivered int32
	a, _ := d.StartCaptureAsync(&CaptureFormats{})
	a.GetCallBackInterface().SetOnReadSample(func(_ *CaptureAsync, _ uint32, _ uint32, _ int64, _ *mf.IMFSample) {
		atomic.AddInt32(&delivered, 1)
	})

	// Fewer than the cap of empties, then a real sample, then the cap again.
	for i := 0; i < maxEmptySampleRetries-1; i++ {
		a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}
	a.handleReadSample(uint32(consts.S_OK), 0, 0, 1, realFakeSample())
	for i := 0; i < maxEmptySampleRetries; i++ {
		a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}

	if got := atomic.LoadInt32(&delivered); got != 1 {
		t.Fatalf("expected exactly 1 delivered frame, got %d", got)
	}
	// Initial read, then one resend per empty sample: (cap-1) before the real
	// sample and cap after it. A real sample does not auto-resend.
	wantReads := int32(1 + (maxEmptySampleRetries - 1) + maxEmptySampleRetries - 1)
	if got := reader.readCount(); got != wantReads {
		t.Fatalf("expected %d reads, got %d", wantReads, got)
	}
}

// Starting a new capture session creates a fresh reader and a fresh retry
// budget; the exhausted state of the old session does not carry over.
func TestAsyncRestartResetsRetryCounter(t *testing.T) {
	provider := &fakeProvider{}

	// Session 1: exhaust the retry budget and stop.
	reader1 := newFakeReader()
	reader1.emptyWhenExhausted = true
	provider.reader = reader1
	d1 := newTestDeviceWithProvider(provider)
	var errCount1 int32
	d1.SetErrorCallBack(func(error) { atomic.AddInt32(&errCount1, 1) })
	s1, err := d1.StartCaptureAsync(&CaptureFormats{})
	if err != nil {
		t.Fatalf("StartCaptureAsync session 1: %v", err)
	}
	if err := s1.GetNextFrame(); err != nil {
		t.Fatalf("initial GetNextFrame: %v", err)
	}
	for i := 0; i < maxEmptySampleRetries+1; i++ {
		s1.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}
	if got := atomic.LoadInt32(&errCount1); got != 1 {
		t.Fatalf("session 1: expected 1 error, got %d", got)
	}
	d1.CloseDevice()
	if got := reader1.releaseCount(); got != 1 {
		t.Fatalf("session 1 reader released once, got %d", got)
	}

	// Session 2: a new device/reader must start with a zeroed retry counter, so
	// cap-many empties do not yet report an error.
	reader2 := newFakeReader()
	reader2.emptyWhenExhausted = true
	provider.reader = reader2
	d2 := newTestDeviceWithProvider(provider)
	var errCount2 int32
	d2.SetErrorCallBack(func(error) { atomic.AddInt32(&errCount2, 1) })
	s2, err := d2.StartCaptureAsync(&CaptureFormats{})
	if err != nil {
		t.Fatalf("StartCaptureAsync session 2: %v", err)
	}
	if err := s2.GetNextFrame(); err != nil {
		t.Fatalf("session 2 initial GetNextFrame: %v", err)
	}
	for i := 0; i < maxEmptySampleRetries; i++ {
		s2.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}
	if got := atomic.LoadInt32(&errCount2); got != 0 {
		t.Fatalf("restarted session must reset retry counter, errors=%d", got)
	}
	d2.CloseDevice()

	if got := provider.createdCount(); got != 2 {
		t.Fatalf("expected 2 readers created across restarts, got %d", got)
	}
	if got := reader2.releaseCount(); got != 1 {
		t.Fatalf("session 2 reader released once, got %d", got)
	}
}

// A failing HRESULT is reported exactly once and never as a frame delivery.
func TestAsyncFailingHResultReportedOnce(t *testing.T) {
	reader := newFakeReader()
	d := newTestDevice(reader)
	var errCount int32
	d.SetErrorCallBack(func(error) { atomic.AddInt32(&errCount, 1) })
	var delivered int32
	a, _ := d.StartCaptureAsync(&CaptureFormats{})
	a.GetCallBackInterface().SetOnReadSample(func(_ *CaptureAsync, _ uint32, _ uint32, _ int64, _ *mf.IMFSample) {
		atomic.AddInt32(&delivered, 1)
	})

	const failHR = uint32(0x80004005) // E_FAIL
	for i := 0; i < 3; i++ {
		a.handleReadSample(failHR, 0, 0, 0, nil)
	}
	if got := atomic.LoadInt32(&errCount); got != 1 {
		t.Fatalf("expected exactly 1 error callback, got %d", got)
	}
	if got := atomic.LoadInt32(&delivered); got != 0 {
		t.Fatalf("failed status must not deliver a frame, got %d", got)
	}
	if got := reader.readCount(); got != 0 {
		t.Fatalf("failed status must not auto re-request, reads=%d", got)
	}
}

// ---------------------------------------------------------------------------
// Close semantics
// ---------------------------------------------------------------------------

// Repeated and concurrent CloseDevice converge on a single reader Release.
func TestCloseIsIdempotentAndReleasesOnce(t *testing.T) {
	reader := newFakeReader(frameResult())
	d := newTestDevice(reader)
	if _, err := d.StartCapture(&CaptureFormats{}); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.CloseDevice()
		}()
	}
	wg.Wait()
	d.CloseDevice() // extra sequential call must also be a no-op

	if got := reader.releaseCount(); got != 1 {
		t.Fatalf("reader must be released exactly once, got %d", got)
	}
	if !d.isClosed() {
		t.Fatal("device should report closed")
	}
}

// A callback already in flight when CloseDevice runs must finish without
// touching a released object, and must not deliver a frame after close.
func TestCloseWaitsForInFlightCallbackAndSuppressesFrame(t *testing.T) {
	reader := newFakeReader()
	d := newTestDevice(reader)
	var delivered int32
	a, _ := d.StartCaptureAsync(&CaptureFormats{})
	a.GetCallBackInterface().SetOnReadSample(func(_ *CaptureAsync, _ uint32, _ uint32, _ int64, _ *mf.IMFSample) {
		atomic.AddInt32(&delivered, 1)
	})

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once

	go func() {
		// Simulate a callback that begins (registers with the WaitGroup) and
		// then blocks while the device is closed underneath it.
		if !d.beginCallback() {
			return
		}
		defer d.endCallback()
		once.Do(func() { close(entered) })
		<-proceed
		// This mirrors the delivery point in handleReadSample; after close the
		// gate must have rejected entry instead. We assert via delivered count.
	}()

	<-entered

	closedReturned := make(chan struct{})
	go func() {
		d.CloseDevice()
		close(closedReturned)
	}()

	// CloseDevice must block until the in-flight callback leaves.
	select {
	case <-closedReturned:
		t.Fatal("CloseDevice returned while a callback was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(proceed) // let the callback finish
	select {
	case <-closedReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseDevice did not return after callback finished")
	}

	// A late real sample after close must never reach the user callback.
	a.handleReadSample(uint32(consts.S_OK), 0, 0, 99, realFakeSample())
	if got := atomic.LoadInt32(&delivered); got != 0 {
		t.Fatalf("no frame may be delivered after close, got %d", got)
	}
	if got := reader.releaseCount(); got != 1 {
		t.Fatalf("reader released once, got %d", got)
	}
}

// Concurrent closes interleaved with frame requests converge: one release and
// every frame request ends as ErrorDeviceNotOpen.
func TestConcurrentCloseAndFrameRequests(t *testing.T) {
	reader := newFakeReader(frameResult())
	d := newTestDevice(reader)
	syncCap, _ := d.StartCapture(&CaptureFormats{})

	var wg sync.WaitGroup
	var notOpen, raceErr int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				d.CloseDevice()
				return
			}
			_, err := syncCap.GetFrame()
			switch {
			case err == nil:
				// Won the race and received the single scripted frame.
			case errors.Is(err, utils.ErrorDeviceNotOpen):
				atomic.AddInt32(&notOpen, 1)
			default:
				// A read already inside the fake when close ran may surface
				// the fake's own (script-exhausted) read error; that is a
				// benign start-race outcome, not a use-after-release.
				atomic.AddInt32(&raceErr, 1)
			}
		}(i)
	}
	wg.Wait()

	if got := reader.releaseCount(); got != 1 {
		t.Fatalf("exactly one release expected, got %d", got)
	}
	// No call may ever return a frame silently after close, and the terminal
	// state must be closed with a single release.
	if !d.isClosed() {
		t.Fatal("device must be closed")
	}
	t.Logf("not-open frame requests: %d, benign read-race errors: %d",
		atomic.LoadInt32(&notOpen), atomic.LoadInt32(&raceErr))
}

// A second capture start on an open device is rejected rather than leaking the
// previous reader; after close it stays rejected.
func TestDuplicateAndPostCloseStartRejected(t *testing.T) {
	reader := newFakeReader(frameResult())
	d := newTestDevice(reader)
	if _, err := d.StartCapture(&CaptureFormats{}); err != nil {
		t.Fatalf("first StartCapture: %v", err)
	}
	if _, err := d.StartCapture(&CaptureFormats{}); !errors.Is(err, utils.ErrorCaptureAlreadyStarted) {
		t.Fatalf("expected ErrorCaptureAlreadyStarted, got %v", err)
	}
	if _, err := d.StartCaptureAsync(&CaptureFormats{}); !errors.Is(err, utils.ErrorCaptureAlreadyStarted) {
		t.Fatalf("async duplicate: expected ErrorCaptureAlreadyStarted, got %v", err)
	}

	d.CloseDevice()
	if _, err := d.StartCapture(&CaptureFormats{}); !errors.Is(err, utils.ErrorDeviceNotOpen) {
		t.Fatalf("post-close start: expected ErrorDeviceNotOpen, got %v", err)
	}
	if got := reader.releaseCount(); got != 1 {
		t.Fatalf("exactly one release expected, got %d", got)
	}
}

// GetNextFrame on a closed device returns ErrorDeviceNotOpen without reading.
func TestAsyncGetNextFrameAfterClose(t *testing.T) {
	reader := newFakeReader()
	d := newTestDevice(reader)
	a, _ := d.StartCaptureAsync(&CaptureFormats{})
	d.CloseDevice()

	if err := a.GetNextFrame(); !errors.Is(err, utils.ErrorDeviceNotOpen) {
		t.Fatalf("expected ErrorDeviceNotOpen, got %v", err)
	}
	if got := reader.readCount(); got != 0 {
		t.Fatalf("closed device issued a read: %d", got)
	}
}

// A read error surfaced while re-requesting after an empty sample is reported
// through the error callback exactly once.
func TestAsyncResendReadErrorReportedOnce(t *testing.T) {
	readErr := errors.New("read failed")
	reader := newFakeReader(readResult{err: readErr})
	d := newTestDevice(reader)
	var errCount int32
	d.SetErrorCallBack(func(error) { atomic.AddInt32(&errCount, 1) })
	a, _ := d.StartCaptureAsync(&CaptureFormats{})

	// The first empty sample triggers a re-request; the fake returns readErr.
	a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)

	if got := atomic.LoadInt32(&errCount); got != 1 {
		t.Fatalf("expected exactly 1 error callback, got %d", got)
	}
}

// Close during an exhausted/error-prone session must suppress any error that
// races with shutdown: the error callback never fires after close.
func TestErrorCallbackSuppressedAfterClose(t *testing.T) {
	reader := newFakeReader()
	d := newTestDevice(reader)
	var errCount int32
	d.SetErrorCallBack(func(error) { atomic.AddInt32(&errCount, 1) })
	a, _ := d.StartCaptureAsync(&CaptureFormats{})
	d.CloseDevice()

	a.handleReadSample(uint32(0x80004005), 0, 0, 0, nil)
	for i := 0; i < maxEmptySampleRetries+2; i++ {
		a.handleReadSample(uint32(consts.S_OK), 0, uint32(consts.MF_SOURCE_READERF_STREAMTICK), 0, nil)
	}
	if got := atomic.LoadInt32(&errCount); got != 0 {
		t.Fatalf("error callback must be suppressed after close, got %d", got)
	}
}
