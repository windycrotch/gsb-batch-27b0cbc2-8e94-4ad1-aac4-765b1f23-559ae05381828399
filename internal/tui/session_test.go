package tui

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Megge06/TermiCam/internal/video"
)

// --- helpers ---------------------------------------------------------------

// fakeSource is an injectable video.FrameSource used instead of a camera.
type fakeSource struct {
	mu       sync.Mutex
	frames   [][]byte
	idx      int
	closed   bool
	closes   int
	blocked  chan struct{}
	readErrs []error
}

func newFakeSource(frameSize int, frameCount int) *fakeSource {
	frames := make([][]byte, frameCount)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{byte(i + 1)}, frameSize)
	}

	return &fakeSource{frames: frames}
}

func (f *fakeSource) ReadFrame(dest []byte) error {
	if f.blocked != nil {
		<-f.blocked
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return video.ErrSessionClosed
	}
	if len(f.readErrs) > 0 {
		err := f.readErrs[0]
		f.readErrs = f.readErrs[1:]
		return err
	}
	if len(f.frames) == 0 {
		return errors.New("fake source exhausted")
	}

	frame := f.frames[f.idx%len(f.frames)]
	f.idx++
	copy(dest, frame)

	return nil
}

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true
	f.closes++

	return nil
}

func (f *fakeSource) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

func (f *fakeSource) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closes
}

func (f *fakeSource) unblock() {
	f.mu.Lock()
	ch := f.blocked
	f.mu.Unlock()

	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// installFakes makes live sessions use fake sources sized from the probed
// resolution and restores every seam on cleanup.
func installFakes(t *testing.T, nativeW, nativeH int) *fakeTracker {
	t.Helper()

	tracker := &fakeTracker{}
	origOpen := openLiveSource
	origRes := probeResolution
	origFPS := probeFramerate
	origName := recordingFilename

	probeResolution = func(video.Device) (int, int, error) { return nativeW, nativeH, nil }
	probeFramerate = func(video.Device, int, int, int) (float64, error) { return 30, nil }
	openLiveSource = func(_ video.Device, _, _, width, height, _ int, _ float64) (video.FrameSource, error) {
		src := newFakeSource(width*height*3, 4)
		tracker.add(src, width, height)
		return src, nil
	}
	recordingFilename = func() string {
		return filepath.Join(t.TempDir(), "rec.tcam")
	}

	t.Cleanup(func() {
		openLiveSource = origOpen
		probeResolution = origRes
		probeFramerate = origFPS
		recordingFilename = origName
	})

	return tracker
}

type fakeTracker struct {
	mu      sync.Mutex
	sources []*fakeSource
	widths  []int
	heights []int
}

func (tr *fakeTracker) add(src *fakeSource, width, height int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	tr.sources = append(tr.sources, src)
	tr.widths = append(tr.widths, width)
	tr.heights = append(tr.heights, height)
}

func (tr *fakeTracker) last() (*fakeSource, int, int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	i := len(tr.sources) - 1
	if i < 0 {
		return nil, 0, 0
	}

	return tr.sources[i], tr.widths[i], tr.heights[i]
}

func (tr *fakeTracker) count() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return len(tr.sources)
}

func newTestModel(t *testing.T) Model {
	t.Helper()

	m := initialModel(nil)
	m.loading = false
	m.screen = screenSelect
	m.devices = []video.Device{{ID: "cam0", Name: "Test Camera"}}
	m.updateListItems()

	return m
}

// enterDevice drives the select screen's Enter handler to open a live session.
func enterDevice(t *testing.T, m Model) Model {
	t.Helper()

	m.screen = screenSelect
	m.selected = 0

	next, _ := m.Update(keyPress("enter"))
	return next.(Model)
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	default:
		return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
	}
}

func writeRecordingFile(t *testing.T, path string, header fileHeader, frames [][]byte) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create recording: %v", err)
	}

	gz := gzip.NewWriter(file)
	if string(header.Magic[:]) == recordingMagic {
		if err := header.writeTo(gz); err != nil {
			t.Fatalf("write header: %v", err)
		}
	} else {
		// Preserve an intentionally bogus magic for corruption tests.
		fields := headerFieldsV2{
			Width: header.Width, Height: header.Height, FPS: header.FPS,
			Color: header.Color, Detailed: header.Detailed, Mirror: header.Mirror,
			Ratio: header.Ratio,
		}
		if _, err := gz.Write(header.Magic[:]); err != nil {
			t.Fatalf("write magic: %v", err)
		}
		if err := binary.Write(gz, binary.LittleEndian, fields); err != nil {
			t.Fatalf("write header: %v", err)
		}
	}
	for i, frame := range frames {
		if _, err := gz.Write(frame); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

func testHeader(width, height, fps int32) fileHeader {
	return fileHeader{
		Magic:  [4]byte{'T', 'C', 'A', '2'},
		Width:  width,
		Height: height,
		FPS:    fps,
	}
}

// writeLegacyRecordingFile writes a recording in the pre-TCA2 format so the
// backward-compatible reader path is exercised.
func writeLegacyRecordingFile(t *testing.T, path string, width, height, fps int32, frames [][]byte) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create recording: %v", err)
	}
	defer func() { _ = file.Close() }()

	gz := gzip.NewWriter(file)
	if _, err := gz.Write([]byte("TCAM")); err != nil {
		t.Fatalf("write magic: %v", err)
	}
	fields := headerFieldsV1{Width: width, Height: height, FPS: fps}
	if err := binary.Write(gz, binary.LittleEndian, fields); err != nil {
		t.Fatalf("write header: %v", err)
	}
	for i, frame := range frames {
		if _, err := gz.Write(frame); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func solidFrames(size, count int, value byte) [][]byte {
	frames := make([][]byte, count)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{value}, size)
	}

	return frames
}

// --- live session lifecycle ------------------------------------------------

func TestEnterLiveSessionBindsFrameSize(t *testing.T) {
	tracker := installFakes(t, 1280, 720)
	m := newTestModel(t)

	m = enterDevice(t, m)

	if m.screen != screenCamera {
		t.Fatalf("screen = %v, want camera", m.screen)
	}
	if m.session == nil {
		t.Fatal("session was not created")
	}
	if m.session.width != 640 || m.session.height != 360 {
		t.Fatalf("frame size = %dx%d, want 640x360", m.session.width, m.session.height)
	}
	src, w, h := tracker.last()
	if src == nil || w != 640 || h != 360 {
		t.Fatalf("source created with %dx%d, want 640x360", w, h)
	}
	if got, want := len(m.session.front), 640*360*3; got != want {
		t.Fatalf("front buffer = %d bytes, want %d", got, want)
	}
	if got, want := len(m.session.back), 640*360*3; got != want {
		t.Fatalf("back buffer = %d bytes, want %d", got, want)
	}
}

func TestEscapeClosesSession(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	src, _, _ := tracker.last()

	next, cmd := m.Update(keyPress("esc"))
	m = next.(Model)

	if cmd != nil {
		t.Fatalf("esc should not schedule a command, got %T", cmd)
	}
	if m.session != nil {
		t.Fatal("session was not cleared on esc")
	}
	if !src.isClosed() {
		t.Fatal("frame source was not closed on esc")
	}
	if m.screen != screenSelect {
		t.Fatalf("screen = %v, want select", m.screen)
	}

}

func TestReenterCreatesFreshSessionWithNewFrameSize(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	first := m.session
	firstID := first.id

	// Leave the view, then re-enter with a different native resolution.
	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)

	probeResolution = func(video.Device) (int, int, error) { return 320, 240, nil }
	m = enterDevice(t, m)

	if m.session == nil {
		t.Fatal("no session after re-entry")
	}
	if m.session.id == firstID {
		t.Fatal("re-entry reused the old session id")
	}
	if m.session == first {
		t.Fatal("re-entry reused the old session")
	}
	if !first.closed {
		t.Fatal("old session was not closed")
	}
	if m.session.width != 320 || m.session.height != 240 {
		t.Fatalf("frame size = %dx%d, want 320x240", m.session.width, m.session.height)
	}
	if got, want := len(m.session.back), 320*240*3; got != want {
		t.Fatalf("back buffer = %d bytes, want %d", got, want)
	}
}

func TestEnterWhileSessionActiveReplacesIt(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	first := m.session
	firstSrc, _, _ := tracker.last()

	// Re-enter without leaving (e.g. stale key press while the view is active).
	m.screen = screenSelect
	m.selected = 0
	next, _ := m.Update(keyPress("enter"))
	m = next.(Model)

	if m.session == nil {
		t.Fatal("no session after re-entry")
	}
	if m.session == first {
		t.Fatal("session was not replaced")
	}
	if !first.closed || !firstSrc.isClosed() {
		t.Fatal("old session/source was not closed before creating the new one")
	}
	if got := tracker.count(); got != 2 {
		t.Fatalf("created %d sources, want 2", got)
	}
}

func TestEnterLiveSessionFailureLeavesNoStaleSession(t *testing.T) {
	origOpen := openLiveSource
	origRes := probeResolution
	origFPS := probeFramerate
	t.Cleanup(func() {
		openLiveSource = origOpen
		probeResolution = origRes
		probeFramerate = origFPS
	})

	probeResolution = func(video.Device) (int, int, error) { return 640, 480, nil }
	probeFramerate = func(video.Device, int, int, int) (float64, error) { return 30, nil }

	failing := true
	created := 0
	openLiveSource = func(video.Device, int, int, int, int, int, float64) (video.FrameSource, error) {
		if failing {
			return nil, errors.New("camera busy")
		}
		created++
		return newFakeSource(640*480*3, 1), nil
	}

	m := newTestModel(t)
	m = enterDevice(t, m)
	if m.err == nil {
		t.Fatal("expected an error when the source cannot be opened")
	}
	if m.session != nil {
		t.Fatal("a failed entry must not leave a session behind")
	}
	if m.screen != screenSelect {
		t.Fatalf("screen = %v, want select", m.screen)
	}

	failing = false
	m.err = nil
	m = enterDevice(t, m)
	if m.session == nil {
		t.Fatal("entry after a failure should succeed")
	}
	if created != 1 {
		t.Fatalf("created %d sources, want 1", created)
	}
}

// --- late frames -----------------------------------------------------------

func TestLateFrameFromPreviousSessionIsDiscarded(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	staleID := m.session.id

	// Simulate a read that was already in flight when the user left.
	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)
	m = enterDevice(t, m)

	newSession := m.session
	newSrc, _, _ := tracker.last()
	_ = newSrc

	frontBefore := newSession.front
	backBefore := newSession.back

	// Deliver the stale frame result.
	next, cmd := m.Update(frameMsg{sessionID: staleID})
	m = next.(Model)

	if cmd != nil {
		t.Fatalf("stale frame should not schedule a read, got %T", cmd)
	}
	if m.session != newSession {
		t.Fatal("stale frame changed the active session")
	}
	if &m.session.front[0] != &frontBefore[0] || &m.session.back[0] != &backBefore[0] {
		t.Fatal("stale frame swapped the new session's buffers")
	}
}

func TestLateErrorFromPreviousSessionDoesNotCloseNewSession(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	staleID := m.session.id

	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)
	m = enterDevice(t, m)

	newSession := m.session
	newSrc, _, _ := tracker.last()

	next, _ = m.Update(frameErrMsg{sessionID: staleID, err: errors.New("stale read failed")})
	m = next.(Model)

	if m.session != newSession {
		t.Fatal("stale error tore down the new session")
	}
	if newSrc.isClosed() {
		t.Fatal("stale error closed the new session's source")
	}
	if m.err != nil {
		t.Fatalf("stale error surfaced to the user: %v", m.err)
	}
}

func TestFrameForClosedSessionIsIgnored(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	id := m.session.id
	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)

	next, cmd := m.Update(frameMsg{sessionID: id})
	m = next.(Model)

	if cmd != nil {
		t.Fatal("frame for a closed session scheduled another read")
	}
	if m.session != nil {
		t.Fatal("frame for a closed session resurrected a session")
	}
}

func TestReadFrameCommandCarriesSessionID(t *testing.T) {
	src := newFakeSource(4*4*3, 1)
	session := newLiveCameraSession(7, src, 4, 4)

	cmd := readFrameCmd(session)
	if cmd == nil {
		t.Fatal("readFrameCmd returned nil")
	}

	msg := cmd()
	frame, ok := msg.(frameMsg)
	if !ok {
		t.Fatalf("cmd() = %T, want frameMsg", msg)
	}
	if frame.sessionID != 7 {
		t.Fatalf("sessionID = %d, want 7", frame.sessionID)
	}
}

func TestReadFrameCommandReportsSourceError(t *testing.T) {
	src := newFakeSource(4, 1)
	src.readErrs = []error{errors.New("capture failed")}
	session := newLiveCameraSession(3, src, 2, 2)

	msg := readFrameCmd(session)()
	errMsg, ok := msg.(frameErrMsg)
	if !ok {
		t.Fatalf("cmd() = %T, want frameErrMsg", msg)
	}
	if errMsg.sessionID != 3 || errMsg.err == nil {
		t.Fatalf("frameErrMsg = %+v", errMsg)
	}
}

func TestStartReadDispatchesOnlyOneReadAtATime(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	if !m.session.reading {
		t.Fatal("entering the view should start a read")
	}

	// A second dispatch while one read is pending must be suppressed so two
	// goroutines never write the same buffer.
	if cmd := m.startRead(); cmd != nil {
		t.Fatal("startRead dispatched a second concurrent read")
	}

	// Once the result arrives another read is allowed.
	id := m.session.id
	next, cmd := m.Update(frameMsg{sessionID: id})
	m = next.(Model)

	if cmd == nil {
		t.Fatal("no follow-up read after the frame arrived")
	}
	if m.session == nil || !m.session.reading {
		t.Fatal("follow-up read was not marked in flight")
	}
}

func TestLateFrameDoesNotUnblockNewSessionRead(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)
	staleID := m.session.id

	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)
	m = enterDevice(t, m)

	// The new session has its own read in flight. A stale result must not
	// clear that flag, otherwise a second read could be dispatched on top.
	if !m.session.reading {
		t.Fatal("new session should have a read in flight")
	}

	next, _ = m.Update(frameMsg{sessionID: staleID})
	m = next.(Model)

	if !m.session.reading {
		t.Fatal("stale frame cleared the new session's in-flight flag")
	}
	if cmd := m.startRead(); cmd != nil {
		t.Fatal("stale frame allowed a concurrent read on the new session")
	}
}

func TestFrameErrorClosesSession(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	id := m.session.id
	src, _, _ := tracker.last()

	next, _ := m.Update(frameErrMsg{sessionID: id, err: errors.New("ffmpeg died")})
	m = next.(Model)

	if m.session != nil {
		t.Fatal("session should be closed after a frame error")
	}
	if !src.isClosed() {
		t.Fatal("source should be closed after a frame error")
	}
	if m.err == nil {
		t.Fatal("frame error should be surfaced")
	}
}

// --- recording lifecycle ---------------------------------------------------

func TestRecordingResetsOnSessionSwitch(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	next, _ := m.Update(keyPress("r"))
	m = next.(Model)

	if !m.isRecording() {
		t.Fatal("recording did not start")
	}
	firstSession := m.session

	// Leave and re-enter.
	next, _ = m.Update(keyPress("esc"))
	m = next.(Model)
	m = enterDevice(t, m)

	if m.session == firstSession {
		t.Fatal("session was not replaced")
	}
	if m.isRecording() {
		t.Fatal("recording state leaked into the new session")
	}
	if m.session.recorder != nil {
		t.Fatal("new session inherited a recorder")
	}
}

func TestRecordingStopsOnExit(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	next, _ := m.Update(keyPress("r"))
	m = next.(Model)
	if !m.isRecording() {
		t.Fatal("recording did not start")
	}

	next, _ = m.Update(keyPress("esc"))
	m = next.(Model)

	if m.isRecording() {
		t.Fatal("recording still active after leaving the view")
	}
}

func TestPlaybackIgnoresRecordKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 10), solidFrames(12, 2, 1))

	m := initialModel([]string{path})
	if m.err != nil {
		t.Fatalf("initialModel: %v", m.err)
	}
	t.Cleanup(m.shutdown)

	next, _ := m.Update(keyPress("r"))
	m = next.(Model)

	if m.isRecording() {
		t.Fatal("recording must not start during playback")
	}
}

func TestRecordingWritesHeaderAndFrames(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	dir := t.TempDir()
	path := filepath.Join(dir, "out.tcam")
	recordingFilename = func() string { return path }

	next, _ := m.Update(keyPress("r"))
	m = next.(Model)
	if !m.isRecording() {
		t.Fatal("recording did not start")
	}

	// Deliver two frames through the session path.
	sessionID := m.session.id
	for i := 0; i < 2; i++ {
		next, _ = m.Update(frameMsg{sessionID: sessionID})
		m = next.(Model)
	}

	next, _ = m.Update(keyPress("esc"))
	_ = next

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("recording is not readable: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if reader.header.Width != 640 || reader.header.Height != 480 {
		t.Fatalf("recorded header = %dx%d, want 640x480", reader.header.Width, reader.header.Height)
	}

	if err := reader.ReadFrame(make([]byte, reader.frameSize)); err != nil {
		t.Fatalf("recording has no readable frame: %v", err)
	}
}

// --- playback --------------------------------------------------------------

func TestPlaybackLoopsAtEndOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.tcam")

	header := testHeader(2, 3, 10)
	writeRecordingFile(t, path, header, [][]byte{
		bytes.Repeat([]byte{0x11}, 18),
		bytes.Repeat([]byte{0x22}, 18),
	})

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback: %v", err)
	}
	defer func() { _ = reader.Close() }()

	buf := make([]byte, reader.frameSize)
	seq := make([]byte, 18)

	for i := 0; i < 6; i++ {
		if err := reader.ReadFrame(buf); err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		copy(seq[i*3:], buf)
	}

	want := []byte{
		0x11, 0x11, 0x11,
		0x22, 0x22, 0x22,
		0x11, 0x11, 0x11,
		0x22, 0x22, 0x22,
		0x11, 0x11, 0x11,
		0x22, 0x22, 0x22,
	}
	if !bytes.Equal(seq, want) {
		t.Fatalf("frame sequence = %v, want %v", seq, want)
	}
}

func TestPlaybackBuffersMatchHeaderSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "size.tcam")
	writeRecordingFile(t, path, testHeader(3, 5, 12), solidFrames(45, 1, 7))

	m := initialModel([]string{path})
	if m.err != nil {
		t.Fatalf("initialModel: %v", m.err)
	}
	t.Cleanup(m.shutdown)

	if m.session == nil || !m.session.playback {
		t.Fatal("playback session was not created")
	}
	if m.session.width != 3 || m.session.height != 5 {
		t.Fatalf("session size = %dx%d, want 3x5", m.session.width, m.session.height)
	}
	if got, want := len(m.session.front), 45; got != want {
		t.Fatalf("front buffer = %d, want %d", got, want)
	}
	if got, want := len(m.session.back), 45; got != want {
		t.Fatalf("back buffer = %d, want %d", got, want)
	}

}

func TestPlaybackUsesHeaderSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.tcam")

	header := fileHeader{
		Magic:    [4]byte{'T', 'C', 'A', '2'},
		Width:    2,
		Height:   2,
		FPS:      12,
		Color:    1,
		Detailed: 1,
		Mirror:   1,
	}
	writeRecordingFile(t, path, header, solidFrames(12, 1, 9))

	m := initialModel(nil)
	m.fps = 30
	m.color = false
	m.paletteMode = "custom"
	m.mirror = false

	if err := m.enterPlaybackSession(path); err != nil {
		t.Fatalf("enterPlaybackSession: %v", err)
	}
	m.screen = screenCamera
	t.Cleanup(m.shutdown)

	if m.fps != 12 || !m.color || m.paletteMode != "detailed" || !m.mirror {
		t.Fatalf("settings not taken from header: fps=%d color=%v palette=%q mirror=%v",
			m.fps, m.color, m.paletteMode, m.mirror)
	}

	// Leaving playback restores the user's own settings.
	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)

	if m.fps != 30 || m.color || m.paletteMode != "custom" || m.mirror {
		t.Fatalf("settings not restored: fps=%d color=%v palette=%q mirror=%v",
			m.fps, m.color, m.paletteMode, m.mirror)
	}
	if m.session != nil {
		t.Fatal("session was not closed after leaving playback")
	}
}

func TestPlaybackExitQuits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quit.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 30), solidFrames(12, 1, 1))

	m := initialModel([]string{path})
	t.Cleanup(m.shutdown)
	_, cmd := m.Update(keyPress("esc"))
	if cmd == nil {
		t.Fatal("leaving playback should quit the program")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cmd() = %T, want tea.QuitMsg", cmd())
	}
}

// --- corrupt recordings ----------------------------------------------------

func TestPlaybackRejectsCorruptHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-magic.tcam")

	header := testHeader(2, 2, 10)
	header.Magic = [4]byte{'X', 'X', 'X', 'X'}
	writeRecordingFile(t, path, header, solidFrames(12, 1, 1))

	_, err := openPlayback(path)
	if err == nil || !strings.Contains(err.Error(), "invalid recording file format") {
		t.Fatalf("openPlayback error = %v, want invalid format", err)
	}
}

func TestPlaybackReadsLegacyRecording(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.tcam")
	writeLegacyRecordingFile(t, path, 2, 2, 10, solidFrames(12, 2, 3))

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback legacy: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if reader.header.Width != 2 || reader.header.Height != 2 || reader.header.FPS != 10 {
		t.Fatalf("legacy header = %dx%d@%d", reader.header.Width, reader.header.Height, reader.header.FPS)
	}
	if reader.header.hasRatio() {
		t.Fatalf("legacy header unexpectedly reports a stored ratio: %v", reader.header.Ratio)
	}
	if err := reader.ReadFrame(make([]byte, reader.frameSize)); err != nil {
		t.Fatalf("ReadFrame legacy: %v", err)
	}
}

func TestPlaybackRatioStoredInHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ratio.tcam")

	header := testHeader(2, 2, 10)
	header.Ratio = 0.6
	writeRecordingFile(t, path, header, solidFrames(12, 2, 1))

	m := initialModel(nil)
	m.ratio = 0.45
	if err := m.enterPlaybackSession(path); err != nil {
		t.Fatalf("enterPlaybackSession: %v", err)
	}
	t.Cleanup(m.shutdown)

	if m.ratio != 0.6 {
		t.Fatalf("playback ratio = %v, want 0.6", m.ratio)
	}

	m.closeSession()
	if m.ratio != 0.45 {
		t.Fatalf("ratio not restored after playback: %v", m.ratio)
	}
}

func TestPlaybackRejectsInvalidDimensions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-dims.tcam")
	writeRecordingFile(t, path, testHeader(0, 4, 10), solidFrames(12, 1, 1))

	_, err := openPlayback(path)
	if err == nil || !strings.Contains(err.Error(), "invalid recording dimensions") {
		t.Fatalf("openPlayback error = %v, want invalid dimensions", err)
	}
}

func TestPlaybackRejectsInvalidFPS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-fps.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 0), solidFrames(12, 1, 1))

	_, err := openPlayback(path)
	if err == nil || !strings.Contains(err.Error(), "invalid recording frame rate") {
		t.Fatalf("openPlayback error = %v, want invalid frame rate", err)
	}
}

func TestPlaybackRejectsNonGzipFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-gzip.tcam")
	if err := os.WriteFile(path, []byte("definitely not gzip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := openPlayback(path)
	if err == nil || !strings.Contains(err.Error(), "not a valid .tcam recording") {
		t.Fatalf("openPlayback error = %v, want gzip error", err)
	}
}

func TestPlaybackTruncatedFrameIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.tcam")

	// Header claims 4x4 RGB24 (48 bytes) but only 10 bytes of frame follow.
	writeRecordingFile(t, path, testHeader(4, 4, 10), [][]byte{bytes.Repeat([]byte{1}, 10)})

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback: %v", err)
	}
	defer func() { _ = reader.Close() }()

	err = reader.ReadFrame(make([]byte, reader.frameSize))
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("ReadFrame error = %v, want truncated", err)
	}
	if !reader.closed {
		t.Fatal("reader must be closed after a truncated frame")
	}
	if reader.file != nil || reader.gz != nil {
		t.Fatal("file and decompressor must be released after a truncated frame")
	}
}

func TestPlaybackTruncatedFirstFrameFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated-first.tcam")
	writeRecordingFile(t, path, testHeader(4, 4, 10), [][]byte{bytes.Repeat([]byte{1}, 5)})

	m := initialModel([]string{path})
	if m.err == nil {
		t.Fatal("initialModel should fail on a truncated first frame")
	}
	if m.session != nil {
		t.Fatal("no session should be created for a broken recording")
	}
}

func TestPlaybackEmptyRecordingIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 10), nil)

	m := initialModel([]string{path})
	if m.err == nil || !strings.Contains(m.err.Error(), "no frame data") {
		t.Fatalf("initialModel error = %v, want no-frame-data", m.err)
	}
	if m.session != nil {
		t.Fatal("no session should be created for an empty recording")
	}
}

func TestPlaybackLoopRejectsChangedFrameSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changing.tcam")

	writeRecordingFile(t, path, testHeader(2, 2, 10), solidFrames(12, 1, 1))

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback: %v", err)
	}
	defer func() { _ = reader.Close() }()

	buf := make([]byte, reader.frameSize)
	if err := reader.ReadFrame(buf); err != nil {
		t.Fatalf("first ReadFrame: %v", err)
	}

	// Rewrite the file with a different frame size before the loop restarts.
	writeRecordingFile(t, path, testHeader(4, 4, 10), solidFrames(48, 1, 2))

	err = reader.ReadFrame(buf)
	if err == nil || !strings.Contains(err.Error(), "frame size changed") {
		t.Fatalf("ReadFrame error = %v, want frame-size-change", err)
	}
	if !reader.closed {
		t.Fatal("reader must be closed when the looped header no longer matches")
	}
}

func TestPlaybackReadAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 10), solidFrames(12, 2, 1))

	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	err = reader.ReadFrame(make([]byte, reader.frameSize))
	if !errors.Is(err, video.ErrSessionClosed) {
		t.Fatalf("ReadFrame after Close = %v, want ErrSessionClosed", err)
	}
}

func TestPlaybackTickFromOldSessionIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tick.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 10), solidFrames(12, 2, 1))

	m := initialModel([]string{path})
	t.Cleanup(m.shutdown)
	staleID := m.session.id

	next, _ := m.Update(keyPress("esc"))
	m = next.(Model)

	// A tick that was already scheduled must not read from a closed session.
	next, cmd := m.Update(playbackTickMsg{sessionID: staleID})
	m = next.(Model)

	if cmd != nil {
		t.Fatalf("stale playback tick scheduled a read: %T", cmd)
	}
	if m.session != nil {
		t.Fatal("stale playback tick resurrected a session")
	}
}

func TestPlaybackTickReadsNextFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tick2.tcam")
	frames := [][]byte{
		bytes.Repeat([]byte{0x01}, 12),
		bytes.Repeat([]byte{0x02}, 12),
	}
	writeRecordingFile(t, path, testHeader(2, 2, 10), frames)

	m := initialModel([]string{path})
	t.Cleanup(m.shutdown)
	id := m.session.id

	next, cmd := m.Update(playbackTickMsg{sessionID: id})
	m = next.(Model)

	if cmd == nil {
		t.Fatal("playback tick should schedule a frame read")
	}

	msg := cmd()
	frame, ok := msg.(frameMsg)
	if !ok {
		t.Fatalf("cmd() = %T, want frameMsg", msg)
	}

	next, _ = m.Update(frame)
	m = next.(Model)

	if m.session.front[0] != 0x02 {
		t.Fatalf("second frame not rendered: first byte %#x", m.session.front[0])
	}
}

func TestPlaybackErrorClosesSessionAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt-mid.tcam")

	// Start from a valid recording so the session opens with one frame read.
	writeRecordingFile(t, path, testHeader(200, 200, 10), solidFrames(200*200*3, 1, 5))

	m := initialModel([]string{path})
	t.Cleanup(m.shutdown)
	if m.err != nil {
		t.Fatalf("initialModel: %v", m.err)
	}

	// Corrupt the recording in place so the loop hits a truncated frame.
	writeRecordingFile(t, path, testHeader(200, 200, 10), [][]byte{bytes.Repeat([]byte{5}, 100)})

	id := m.session.id
	reader, ok := m.session.source.(*playbackReader)
	if !ok {
		t.Fatalf("source = %T, want *playbackReader", m.session.source)
	}

	next, cmd := m.Update(playbackTickMsg{sessionID: id})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("playback tick should schedule a read")
	}

	msg := cmd()
	if _, isErr := msg.(frameErrMsg); !isErr {
		t.Fatalf("expected a frame error, got %T", msg)
	}

	next, _ = m.Update(msg)
	m = next.(Model)

	if m.session != nil {
		t.Fatal("playback session should be closed after an error")
	}
	if m.err == nil {
		t.Fatal("playback error should be surfaced")
	}
	if !reader.closed || reader.file != nil || reader.gz != nil {
		t.Fatal("file and decompressor must be released after a playback error")
	}
}

func TestPlaybackEscapeRestoresSettingsBeforeQuitting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restore-esc.tcam")

	header := testHeader(2, 2, 10)
	header.Color = 1
	header.Mirror = 1
	header.Ratio = 0.7
	writeRecordingFile(t, path, header, solidFrames(12, 2, 1))

	m := initialModel(nil)
	m.fps = 20
	m.color = false
	m.paletteMode = "custom"
	m.customPalette = "@@"
	m.mirror = false
	m.ratio = 0.5

	if err := m.enterPlaybackSession(path); err != nil {
		t.Fatalf("enterPlaybackSession: %v", err)
	}
	m.screen = screenCamera

	if m.ratio != 0.7 {
		t.Fatalf("playback ratio = %v, want 0.7", m.ratio)
	}

	next, cmd := m.Update(keyPress("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("leaving playback should quit")
	}

	if m.fps != 20 || m.color || m.paletteMode != "custom" || m.mirror || m.ratio != 0.5 {
		t.Fatalf("settings not restored on esc: fps=%d color=%v palette=%q mirror=%v ratio=%v",
			m.fps, m.color, m.paletteMode, m.mirror, m.ratio)
	}
	if m.session != nil {
		t.Fatal("session still active after esc")
	}
}

// --- shutdown --------------------------------------------------------------

func TestQuitReleasesSessionAndRecording(t *testing.T) {
	tracker := installFakes(t, 640, 480)
	m := newTestModel(t)
	m = enterDevice(t, m)

	next, _ := m.Update(keyPress("r"))
	m = next.(Model)
	src, _, _ := tracker.last()

	next, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = next.(Model)

	if cmd == nil {
		t.Fatal("quit should return tea.Quit")
	}
	if m.session != nil {
		t.Fatal("session not released on quit")
	}
	if !src.isClosed() {
		t.Fatal("source not closed on quit")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	installFakes(t, 640, 480)
	m := newTestModel(t)

	m.shutdown()
	m.shutdown()

	if m.session != nil {
		t.Fatal("shutdown created a session")
	}
}

func TestCloseSessionRestoresPlaybackSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restore.tcam")
	writeRecordingFile(t, path, testHeader(2, 2, 15), solidFrames(12, 1, 1))

	m := initialModel(nil)
	m.fps = 24
	m.color = false
	m.paletteMode = "default"
	m.mirror = false

	if err := m.enterPlaybackSession(path); err != nil {
		t.Fatalf("enterPlaybackSession: %v", err)
	}
	t.Cleanup(m.shutdown)

	m.closeSession()

	if m.fps != 24 || m.paletteMode != "default" || m.color || m.mirror {
		t.Fatalf("settings not restored: fps=%d palette=%q color=%v mirror=%v",
			m.fps, m.paletteMode, m.color, m.mirror)
	}
	if m.playbackRestore != nil {
		t.Fatal("restore snapshot was not cleared")
	}
}

// --- concurrency -----------------------------------------------------------

func TestConcurrentCloseAndReadAreSafe(t *testing.T) {
	src := newFakeSource(4*4*3, 3)
	src.blocked = make(chan struct{})

	session := newLiveCameraSession(1, src, 4, 4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = session.source.ReadFrame(session.back)
	}()

	// Close while the read is in flight; this must not deadlock or panic.
	done := make(chan struct{})
	go func() {
		session.close()
		close(done)
	}()

	src.unblock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close deadlocked with an in-flight read")
	}

	wg.Wait()

	if !session.isClosed() {
		t.Fatal("session not marked closed")
	}
	if !src.isClosed() {
		t.Fatal("source not closed")
	}
}

func TestSessionCloseStopsRecorder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec-close.tcam")

	src := newFakeSource(12, 1)
	session := newLiveCameraSession(1, src, 2, 2)
	if err := session.startRecording(path, testHeader(2, 2, 10)); err != nil {
		t.Fatalf("startRecording: %v", err)
	}
	if !session.isRecording() {
		t.Fatal("recording did not start")
	}

	session.close()

	if session.isRecording() {
		t.Fatal("recorder still active after close")
	}
	if !src.isClosed() {
		t.Fatal("source not closed")
	}

	// The partial recording of zero frames is still a valid, readable file.
	reader, err := openPlayback(path)
	if err != nil {
		t.Fatalf("openPlayback after close: %v", err)
	}
	_ = reader.Close()
}
