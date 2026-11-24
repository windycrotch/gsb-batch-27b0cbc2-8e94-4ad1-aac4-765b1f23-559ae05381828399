// Package oss provides access to OSS (Open Sound System) audio interface.
package oss

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrNotPlayback is returned by Write when the device was not opened for playback.
var ErrNotPlayback = errors.New("oss: device not opened for playback")

// ErrNotCapture is returned by Read when the device was not opened for recording.
var ErrNotCapture = errors.New("oss: device not opened for recording")

// ErrClosed is returned by any Audio method called after the device was closed.
// It also matches os.ErrClosed via errors.Is.
var ErrClosed = fmt.Errorf("oss: device closed: %w", os.ErrClosed)

// audioFile is the part of *os.File used by Audio. It is an interface so the
// state machine can be exercised with a fake device in tests.
type audioFile interface {
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	Fd() uintptr
	Close() error
}

type ioctlFunc func(fd uintptr, req uint, val int) (int, error)

type ioctlInfoFunc func(fd uintptr, req uint) (audioBufInfo, error)

// Audio represents an audio device.
//
// An Audio is safe for concurrent use. State changes from Start, Reset, Write,
// Read and Close are serialized so that Playing and Recording always describe
// the last completed operation. In particular, once Close returns no method
// touches the underlying file descriptor anymore.
type Audio struct {
	file audioFile
	mode int

	// mu guards closed, playing and recording. It is held for the duration of
	// every ioctl so an ioctl can never be issued on a closed descriptor.
	// Write and Read only take it to check/publish state, never across the
	// blocking I/O itself.
	mu        sync.Mutex
	closed    bool
	playing   bool
	recording bool

	ioctlFn  ioctlFunc
	ioctlIFn ioctlInfoFunc
}

func newAudio(file audioFile, mode int) *Audio {
	return &Audio{
		file:     file,
		mode:     mode,
		ioctlFn:  ioctl,
		ioctlIFn: ioctlI,
	}
}

func audioPath(path []string) string {
	var devPath string
	if env := os.Getenv("OSS_AUDIODEV"); env != "" {
		devPath = env
	}
	if env := os.Getenv("AUDIODEV"); env != "" {
		devPath = env
	}
	if len(path) > 0 {
		devPath = path[0]
	}
	if devPath == "" {
		devPath = "/dev/dsp"
	}

	return devPath
}

func openAudio(mode int, path []string) (*Audio, error) {
	file, err := os.OpenFile(audioPath(path), mode, 0664)
	if err != nil {
		return nil, err
	}

	if mode == os.O_RDWR {
		if _, err := ioctl(file.Fd(), sndctlDspSetduplex, 0); err != nil {
			file.Close()
			return nil, err
		}
	}

	return newAudio(file, mode), nil
}

// OpenAudio opens the audio device for playback.
func OpenAudio(path ...string) (*Audio, error) {
	return openAudio(os.O_WRONLY, path)
}

// OpenCapture opens the audio device for recording.
func OpenCapture(path ...string) (*Audio, error) {
	return openAudio(os.O_RDONLY, path)
}

// OpenDuplex opens the audio device for simultaneous playback and recording.
func OpenDuplex(path ...string) (*Audio, error) {
	return openAudio(os.O_RDWR, path)
}

// normErr maps the errors produced by a closed *os.File to ErrClosed so callers
// get one recognizable error whether the close happened before or during a call.
func normErr(err error) error {
	if err != nil && errors.Is(err, os.ErrClosed) {
		return ErrClosed
	}

	return err
}

// doIoctl runs an ioctl while holding the device lock, so it can never race
// with Close and never touches a closed file descriptor.
func (a *Audio) doIoctl(req uint, val int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return 0, ErrClosed
	}

	value, err := a.ioctlFn(a.file.Fd(), req, val)

	return value, normErr(err)
}

// doIoctlI is doIoctl for ioctls that fill an audioBufInfo.
func (a *Audio) doIoctlI(req uint) (audioBufInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return audioBufInfo{}, ErrClosed
	}

	info, err := a.ioctlIFn(a.file.Fd(), req)

	return info, normErr(err)
}

// checkOpen reports ErrClosed if the device has been closed.
func (a *Audio) checkOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return ErrClosed
	}

	return nil
}

// Close closes the audio device.
//
// After Close returns, every other method returns ErrClosed. Calling Close more
// than once is safe and returns ErrClosed as well.
func (a *Audio) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrClosed
	}
	a.closed = true
	a.playing = false
	a.recording = false
	a.mu.Unlock()

	// No ioctl can start now (they check a.closed under a.mu) and an in-flight
	// ioctl holds a.mu, so the descriptor is no longer used by this package.
	// *os.File itself makes concurrent Read/Write against Close safe.
	return a.file.Close()
}

// Caps returns the capability mask of an audio device.
func (a *Audio) Caps() (int, error) {
	return a.doIoctl(sndctlDspGetcaps, 0)
}

// IsCap checks if capability is supported.
func (a *Audio) IsCap(c Cap) (bool, error) {
	caps, err := a.Caps()
	if err != nil {
		return false, err
	}

	if caps&int(c) != 0 {
		return true, nil
	}

	return false, nil
}

// Formats returns the supported formats mask.
func (a *Audio) Formats() (int, error) {
	return a.doIoctl(sndctlDspGetfmts, 0)
}

// IsFormat checks if format is supported.
func (a *Audio) IsFormat(format Format) (bool, error) {
	formats, err := a.Formats()
	if err != nil {
		return false, err
	}

	if formats&int(format) != 0 {
		return true, nil
	}

	return false, nil
}

// Playing checks if audio is playing.
func (a *Audio) Playing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.playing
}

// Recording checks if audio is being recorded.
func (a *Audio) Recording() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.recording
}

// Nonblock forces non-blocking mode regardless of the O_NONBLOCK file mode flag.
func (a *Audio) Nonblock() (int, error) {
	return a.doIoctl(sndctlDspNonblock, 0)
}

// Format sets the audio format.
func (a *Audio) Format(format Format) (Format, error) {
	value, err := a.doIoctl(sndctlDspSetfmt, int(format))

	return Format(value), err
}

// Samplerate sets the audio samplerate (speed).
//
// In some cases it's not possible to support the samplerate requested.
// In that case the nearest possible sampling rate will be used.
func (a *Audio) Samplerate(samplerate int) (int, error) {
	return a.doIoctl(sndctlDspSpeed, samplerate)
}

// Channels sets the number of audio channels.
//
// The returned value may be different from the one requested.
func (a *Audio) Channels(channels int) (int, error) {
	return a.doIoctl(sndctlDspChannels, channels)
}

// SetBufferSize sets the buffer size hint.
// This call just sets the size hint, there is no guarantee that the requested size gets used.
//
// fragments is the maximum number of fragments to be allocated. It can be between 2 and 0x7fff (unlimited).
// The actual number of fragments may be less than requested if there is not enough memory available.
//
// fragsize is the fragment size selector, i.e. for selector N the size is 1<<N.
func (a *Audio) SetBufferSize(fragments, fragsize int) error {
	arg := (fragments << 16) | fragsize
	_, err := a.doIoctl(sndctlDspSetfragment, arg)

	return err
}

// BufferSize returns the buffer size.
//
// For a playback device this is the total playback buffer, for a capture-only
// device it is the total recording buffer.
func (a *Audio) BufferSize() (int, error) {
	if err := a.checkOpen(); err != nil {
		return 0, err
	}

	req := sndctlDspGetospace
	if a.mode == os.O_RDONLY {
		req = sndctlDspGetispace
	}

	info, err := a.doIoctlI(req)
	if err != nil {
		return 0, err
	}

	return int(info.fragstotal * info.fragsize), nil
}

// RecordedBufferSize returns the number of bytes that can be read from the device.
//
// It returns ErrNotCapture on a playback-only device.
func (a *Audio) RecordedBufferSize() (int, error) {
	if err := a.checkOpen(); err != nil {
		return 0, err
	}

	if a.mode == os.O_WRONLY {
		return 0, ErrNotCapture
	}

	info, err := a.doIoctlI(sndctlDspGetispace)
	if err != nil {
		return 0, err
	}

	return int(info.bytes), nil
}

// UnplayedBufferSize returns the size of buffer that is not yet played.
//
// It returns ErrNotPlayback on a capture-only device, where there is no
// playback buffer to query.
func (a *Audio) UnplayedBufferSize() (int, error) {
	if err := a.checkOpen(); err != nil {
		return 0, err
	}

	if a.mode == os.O_RDONLY {
		return 0, ErrNotPlayback
	}

	info, err := a.doIoctlI(sndctlDspGetospace)
	if err != nil {
		return 0, err
	}

	return int(info.fragstotal*info.fragsize - info.bytes), nil
}

// Delay returns the playback buffering delay.
//
// This call tells how long it's going to take before the next sample to be written gets played by the hardware.
// The delay is returned in bytes, it can be converted to seconds by dividing it by:
//
//	samplerate*channels*sampleSizeInBytes.
func (a *Audio) Delay() (int, error) {
	return a.doIoctl(sndctlDspGetodelay, 0)
}

// Start starts audio playback and/or recording, depending on how the device was opened.
//
// Note that the Write and Read calls will automatically start the actual operation.
func (a *Audio) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return ErrClosed
	}

	var trigger int
	if a.mode != os.O_WRONLY {
		trigger |= pcmEnableInput
	}
	if a.mode != os.O_RDONLY {
		trigger |= pcmEnableOutput
	}

	if _, err := a.ioctlFn(a.file.Fd(), sndctlDspSettrigger, trigger); err != nil {
		return normErr(err)
	}

	a.playing = a.mode != os.O_RDONLY
	a.recording = a.mode != os.O_WRONLY

	return nil
}

// Write queues one audio buffer to the hardware.
func (a *Audio) Write(b []byte) (int, error) {
	if err := a.checkOpen(); err != nil {
		return 0, err
	}

	if a.mode == os.O_RDONLY {
		return 0, ErrNotPlayback
	}

	n, err := a.file.Write(b)
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return n, ErrClosed
		}

		// Do not resurrect stale state if Close won the race.
		a.mu.Lock()
		if !a.closed {
			a.playing = false
		}
		a.mu.Unlock()

		return n, err
	}

	return n, nil
}

// Read reads one audio buffer from the hardware.
func (a *Audio) Read(b []byte) (int, error) {
	if err := a.checkOpen(); err != nil {
		return 0, err
	}

	if a.mode == os.O_WRONLY {
		return 0, ErrNotCapture
	}

	n, err := a.file.Read(b)
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return n, ErrClosed
		}

		a.mu.Lock()
		if !a.closed {
			a.recording = false
		}
		a.mu.Unlock()

		return n, err
	}

	return n, nil
}

// Post tells the driver to start playing buffered samples immediately without
// waiting for the buffer to fill, for example after a pause or between segments.
func (a *Audio) Post() error {
	_, err := a.doIoctl(sndctlDspPost, 0)

	return err
}

// Reset aborts audio playback operation.
//
// There is no need to use this call after opening the device.
// The Open operation automatically initializes the device.
//
// On success Playing and Recording both report false.
func (a *Audio) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return ErrClosed
	}

	if _, err := a.ioctlFn(a.file.Fd(), sndctlDspReset, 0); err != nil {
		return normErr(err)
	}

	a.playing = false
	a.recording = false

	if _, err := a.ioctlFn(a.file.Fd(), sndctlDspSettrigger, 0); err != nil {
		return normErr(err)
	}

	return nil
}

// Sync can be used to wait until all samples written to the audio device have been played.
//
// The sync operation will always cause a more or less noticeable pause or click in the output.
// It is recommended to output few samples of silence before and after making this call.
//
// Closing the device will perform the sync operation automatically so using this ioctl call
// is unnecessary before calling Close.
func (a *Audio) Sync() error {
	_, err := a.doIoctl(sndctlDspSync, 0)

	return err
}

// Mixer represents mixer device.
type Mixer struct {
	file *os.File
}

// OpenMixer opens the mixer device.
func OpenMixer(path ...string) (*Mixer, error) {
	dev := &Mixer{}

	var devPath string
	env := os.Getenv("OSS_MIXERDEV")
	if env != "" {
		devPath = env
	}
	env = os.Getenv("MIXERDEV")
	if env != "" {
		devPath = env
	}
	if len(path) > 0 {
		devPath = path[0]
	}
	if devPath == "" {
		devPath = "/dev/mixer"
	}

	file, err := os.OpenFile(devPath, os.O_RDWR, 0664)
	if err != nil {
		return nil, err
	}

	dev.file = file

	return dev, nil
}

// Close closes the mixer device.
func (m *Mixer) Close() error {
	return m.file.Close()
}

// Controls returns a bitmask specifying all available mixer controls.
//
// To determine if, for example, mixer supports a PCM control:
//
//	if controls & (1 << int(oss.SoundMixerPcm)) != 0 { // true
func (m *Mixer) Controls() (int, error) {
	value, err := ioctl(m.file.Fd(), soundMixerReadDevmask, 0)

	return value, err
}

// IsControl checks if control exists.
func (m *Mixer) IsControl(control Control) (bool, error) {
	controls, err := m.Controls()
	if err != nil {
		return false, err
	}

	if controls&(1<<int(control)) != 0 {
		return true, nil
	}

	return false, nil
}

// SetVolume sets the volume for a given mixer control.
func (m *Mixer) SetVolume(control Control, leftVol, rightVol int) (int, int, error) {
	volume := (rightVol << 8) | leftVol

	value, err := ioctl(m.file.Fd(), soundMixerWrite(control), volume)

	left := value & 0xff
	right := (value & 0xff00) >> 8

	return left, right, err
}

// Volume returns the volume of a given mixer control.
func (m *Mixer) Volume(control Control) (int, int, error) {
	value, err := ioctl(m.file.Fd(), soundMixerRead(control), 0)

	left := value & 0xff
	right := (value & 0xff00) >> 8

	return left, right, err
}
