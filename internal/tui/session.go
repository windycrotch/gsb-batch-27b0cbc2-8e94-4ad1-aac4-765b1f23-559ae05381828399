package tui

import (
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Megge06/TermiCam/internal/video"
)

// fileHeader is written atop every .tcam recording so playback can restore the
// settings the recording was captured with.
//
// Recordings written by older versions used the "TCAM" magic and stopped after
// Mirror; newer recordings use "TCA2" and append the character aspect ratio.
// Both are read, so existing recordings keep working.
type fileHeader struct {
	Magic    [4]byte
	Width    int32
	Height   int32
	FPS      int32
	Color    byte
	Detailed byte
	Mirror   byte
	// Ratio is only stored in TCA2 files. Zero means "not recorded".
	Ratio float64
}

const (
	legacyRecordingMagic = "TCAM"
	recordingMagic       = "TCA2"

	// maxPlaybackDimension bounds the values read from a (possibly corrupt)
	// header before any arithmetic is performed on them.
	maxPlaybackDimension = 16384
	// maxPlaybackFrameBytes bounds the frame buffer allocated from a header so
	// a corrupt header cannot make the process allocate absurd amounts of memory.
	maxPlaybackFrameBytes = 64 << 20
)

// openRecordingFile is a seam so playback looping can be tested deterministically.
var openRecordingFile = os.Open

func boolToByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func byteToBool(b byte) bool {
	return b != 0
}

// writeTo serialises the header in the current TCA2 layout.
func (h fileHeader) writeTo(w io.Writer) error {
	fields := headerFieldsV2{
		Width:    h.Width,
		Height:   h.Height,
		FPS:      h.FPS,
		Color:    h.Color,
		Detailed: h.Detailed,
		Mirror:   h.Mirror,
		Ratio:    h.Ratio,
	}

	if _, err := w.Write([]byte(recordingMagic)); err != nil {
		return fmt.Errorf("failed to write recording header: %w", err)
	}
	if err := binary.Write(w, binary.LittleEndian, fields); err != nil {
		return fmt.Errorf("failed to write recording header: %w", err)
	}

	return nil
}

// headerFieldsV2 is the header body of a TCA2 recording (magic excluded).
type headerFieldsV2 struct {
	Width    int32
	Height   int32
	FPS      int32
	Color    byte
	Detailed byte
	Mirror   byte
	Ratio    float64
}

// headerFieldsV1 is the header body of a legacy TCAM recording (magic excluded).
type headerFieldsV1 struct {
	Width    int32
	Height   int32
	FPS      int32
	Color    byte
	Detailed byte
	Mirror   byte
}

// readFileHeader parses and validates the recording header. Both the legacy
// "TCAM" layout and the current "TCA2" layout are accepted.
func readFileHeader(r io.Reader) (fileHeader, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fileHeader{}, fmt.Errorf("failed to read recording header: %w", err)
	}

	header := fileHeader{Magic: magic}

	switch string(magic[:]) {
	case recordingMagic:
		var fields headerFieldsV2
		if err := binary.Read(r, binary.LittleEndian, &fields); err != nil {
			return header, fmt.Errorf("failed to read recording header: %w", err)
		}
		header.Width, header.Height, header.FPS = fields.Width, fields.Height, fields.FPS
		header.Color, header.Detailed, header.Mirror = fields.Color, fields.Detailed, fields.Mirror
		header.Ratio = fields.Ratio
	case legacyRecordingMagic:
		var fields headerFieldsV1
		if err := binary.Read(r, binary.LittleEndian, &fields); err != nil {
			return header, fmt.Errorf("failed to read recording header: %w", err)
		}
		header.Width, header.Height, header.FPS = fields.Width, fields.Height, fields.FPS
		header.Color, header.Detailed, header.Mirror = fields.Color, fields.Detailed, fields.Mirror
	default:
		return header, fmt.Errorf("invalid recording file format: unexpected magic %q", string(magic[:]))
	}

	if header.Width <= 0 || header.Height <= 0 ||
		header.Width > maxPlaybackDimension || header.Height > maxPlaybackDimension {
		return header, fmt.Errorf("invalid recording dimensions %dx%d", header.Width, header.Height)
	}
	if int64(header.Width)*int64(header.Height)*3 > maxPlaybackFrameBytes {
		return header, fmt.Errorf("recording frame size %dx%d exceeds the supported maximum", header.Width, header.Height)
	}
	if header.FPS <= 0 || header.FPS > 1000 {
		return header, fmt.Errorf("invalid recording frame rate %d", header.FPS)
	}
	if header.Ratio < 0 {
		return header, fmt.Errorf("invalid recording aspect ratio %v", header.Ratio)
	}

	return header, nil
}

// hasRatio reports whether the recording stored an aspect ratio. Legacy
// recordings do not, so playback leaves the user's ratio untouched for them.
func (h fileHeader) hasRatio() bool { return h.Ratio > 0 }

// recorder streams frames into a gzip-compressed .tcam file.
type recorder struct {
	file *os.File
	gz   *gzip.Writer
}

func newRecorder(path string, header fileHeader) (*recorder, error) {
	// #nosec G304 -- Path is generated internally from a timestamp.
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	gw := gzip.NewWriter(file)
	if err := header.writeTo(gw); err != nil {
		_ = gw.Close()
		_ = file.Close()
		return nil, err
	}

	return &recorder{file: file, gz: gw}, nil
}

func (r *recorder) writeFrame(p []byte) error {
	if r.gz == nil {
		return errors.New("recorder is closed")
	}
	if _, err := r.gz.Write(p); err != nil {
		return fmt.Errorf("failed to write recorded frame: %w", err)
	}

	return nil
}

func (r *recorder) close() error {
	var errs []error
	if r.gz != nil {
		errs = append(errs, r.gz.Close())
		r.gz = nil
	}
	if r.file != nil {
		errs = append(errs, r.file.Close())
		r.file = nil
	}

	return errors.Join(errs...)
}

// playbackReader loops a .tcam file, validating the header on every loop so the
// frame size always matches the file it is reading from.
type playbackReader struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	gz         *gzip.Reader
	header     fileHeader
	frameSize  int
	framesRead int
	closed     bool
}

func openPlayback(path string) (*playbackReader, error) {
	// #nosec G304 -- Path comes from the command line; opening it is the intent.
	file, err := openRecordingFile(path)
	if err != nil {
		return nil, err
	}

	gz, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("not a valid .tcam recording: %w", err)
	}

	header, err := readFileHeader(gz)
	if err != nil {
		_ = gz.Close()
		_ = file.Close()
		return nil, err
	}

	return &playbackReader{
		path:      path,
		file:      file,
		gz:        gz,
		header:    header,
		frameSize: int(header.Width) * int(header.Height) * 3,
	}, nil
}

// ReadFrame fills dest with one frame, looping back to the first frame when the
// end of the file is reached at a frame boundary. A short read (truncated frame
// data) is a hard error: the reader closes its file and decompressor and the
// caller must stop rendering.
func (p *playbackReader) ReadFrame(dest []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return video.ErrSessionClosed
	}
	if len(dest) < p.frameSize {
		return fmt.Errorf("destination buffer too small: got %d bytes, need at least %d", len(dest), p.frameSize)
	}

	n, err := io.ReadFull(p.gz, dest[:p.frameSize])
	if err == nil {
		p.framesRead++
		return nil
	}

	// A clean EOF at a frame boundary means the file ended exactly on a frame:
	// loop back to the beginning.
	if errors.Is(err, io.EOF) && n == 0 {
		if p.framesRead == 0 {
			return p.fail(fmt.Errorf("recording %q contains no frame data", p.path))
		}
		return p.rewindLocked(dest)
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return p.fail(fmt.Errorf(
			"recording %q is truncated: frame %d has only %d of %d bytes",
			p.path, p.framesRead, n, p.frameSize,
		))
	}

	return p.fail(fmt.Errorf("failed to read frame %d of %q: %w", p.framesRead+1, p.path, err))
}

// rewindLocked restarts the file and reads the first frame. p.mu must be held.
func (p *playbackReader) rewindLocked(dest []byte) error {
	if err := p.reopenLocked(); err != nil {
		return p.fail(fmt.Errorf("failed to loop recording %q: %w", p.path, err))
	}

	n, err := io.ReadFull(p.gz, dest[:p.frameSize])
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return p.fail(fmt.Errorf(
				"recording %q is truncated: frame 1 has only %d of %d bytes",
				p.path, n, p.frameSize,
			))
		}
		return p.fail(fmt.Errorf("failed to read the first frame of %q: %w", p.path, err))
	}

	p.framesRead = 1

	return nil
}

// reopenLocked closes and reopens the underlying file and decompressor. The new
// header must describe exactly the same frame layout, otherwise playback would
// render a buffer whose size no longer matches the file.
func (p *playbackReader) reopenLocked() error {
	if p.gz != nil {
		_ = p.gz.Close()
		p.gz = nil
	}
	if p.file != nil {
		_ = p.file.Close()
		p.file = nil
	}

	// #nosec G304 -- Path was validated by opening it initially.
	file, err := openRecordingFile(p.path)
	if err != nil {
		return err
	}

	gz, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("not a valid .tcam recording: %w", err)
	}

	header, err := readFileHeader(gz)
	if err != nil {
		_ = gz.Close()
		_ = file.Close()
		return err
	}

	if header.Width != p.header.Width || header.Height != p.header.Height {
		_ = gz.Close()
		_ = file.Close()
		return fmt.Errorf(
			"frame size changed while looping: expected %dx%d, got %dx%d",
			p.header.Width, p.header.Height, header.Width, header.Height,
		)
	}

	p.file = file
	p.gz = gz

	return nil
}

// fail marks the reader permanently closed and releases the file and
// decompressor before returning err.
func (p *playbackReader) fail(err error) error {
	p.closed = true
	if p.gz != nil {
		_ = p.gz.Close()
		p.gz = nil
	}
	if p.file != nil {
		_ = p.file.Close()
		p.file = nil
	}

	return err
}

// Close releases the file and decompressor. It is idempotent.
func (p *playbackReader) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	var errs []error
	if p.gz != nil {
		errs = append(errs, p.gz.Close())
		p.gz = nil
	}
	if p.file != nil {
		errs = append(errs, p.file.Close())
		p.file = nil
	}

	return errors.Join(errs...)
}

// cameraSession owns everything tied to one visit to the camera view: the frame
// source, its buffers, the recording file and the playback override settings.
// It is replaced wholesale on every entry so no state can leak into the next
// session.
type cameraSession struct {
	id       uint64
	source   video.FrameSource
	width    int
	height   int
	playback bool
	header   fileHeader
	front    []byte
	back     []byte
	recorder *recorder
	closed   bool
	// reading records that a frame read is in flight. It is only touched from
	// the Update goroutine, so no lock is needed: it stops a second read from
	// being dispatched into the same buffer while the first is still pending.
	reading bool
}

func newLiveCameraSession(id uint64, source video.FrameSource, width, height int) *cameraSession {
	size := width * height * 3

	return &cameraSession{
		id:     id,
		source: source,
		width:  width,
		height: height,
		front:  make([]byte, size),
		back:   make([]byte, size),
	}
}

func newPlaybackCameraSession(id uint64, reader *playbackReader) *cameraSession {
	width := int(reader.header.Width)
	height := int(reader.header.Height)
	size := width * height * 3

	return &cameraSession{
		id:       id,
		source:   reader,
		width:    width,
		height:   height,
		playback: true,
		header:   reader.header,
		front:    make([]byte, size),
		back:     make([]byte, size),
	}
}

func (s *cameraSession) isClosed() bool { return s == nil || s.closed }

// swapBuffers promotes the freshly read frame and exposes the old one as the
// destination for the next read.
func (s *cameraSession) swapBuffers() {
	s.front, s.back = s.back, s.front
}

// close stops the recorder, releases the frame source and marks the session as
// closed. It must be idempotent: Esc, session switches and program exit can all
// race to close the same session.
func (s *cameraSession) close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true

	_ = s.stopRecording()
	if s.source != nil {
		_ = s.source.Close()
	}
}

func (s *cameraSession) isRecording() bool { return s != nil && s.recorder != nil }

func (s *cameraSession) startRecording(path string, header fileHeader) error {
	if s == nil || s.closed {
		return video.ErrSessionClosed
	}
	if s.recorder != nil {
		return nil
	}

	rec, err := newRecorder(path, header)
	if err != nil {
		return err
	}
	s.recorder = rec

	return nil
}

func (s *cameraSession) stopRecording() error {
	if s == nil || s.recorder == nil {
		return nil
	}

	rec := s.recorder
	s.recorder = nil

	return rec.close()
}

func (s *cameraSession) writeRecordedFrame() error {
	if s == nil || s.recorder == nil {
		return nil
	}

	return s.recorder.writeFrame(s.front)
}
