// Package oss provides access to OSS (Open Sound System) audio interface.
package oss

import (
	"errors"
	"os"
)

// ErrNotPlayback is returned by Write when the device was not opened for playback.
var ErrNotPlayback = errors.New("oss: device not opened for playback")

// ErrNotCapture is returned by Read when the device was not opened for recording.
var ErrNotCapture = errors.New("oss: device not opened for recording")

// Audio represents an audio device.
type Audio struct {
	file      *os.File
	mode      int
	playing   bool
	recording bool
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

	return &Audio{file: file, mode: mode}, nil
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

// Close closes the audio device.
func (a *Audio) Close() error {
	a.playing = false
	a.recording = false

	return a.file.Close()
}

// Caps returns the capability mask of an audio device.
func (a *Audio) Caps() (int, error) {
	value, err := ioctl(a.file.Fd(), sndctlDspGetcaps, 0)

	return value, err
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
	value, err := ioctl(a.file.Fd(), sndctlDspGetfmts, 0)

	return value, err
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
	return a.playing
}

// Recording checks if audio is being recorded.
func (a *Audio) Recording() bool {
	return a.recording
}

// Nonblock forces non-blocking mode regardless of the O_NONBLOCK file mode flag.
func (a *Audio) Nonblock() (int, error) {
	value, err := ioctl(a.file.Fd(), sndctlDspNonblock, 0)

	return value, err
}

// Format sets the audio format.
func (a *Audio) Format(format Format) (Format, error) {
	value, err := ioctl(a.file.Fd(), sndctlDspSetfmt, int(format))

	return Format(value), err
}

// Samplerate sets the audio samplerate (speed).
//
// In some cases it's not possible to support the samplerate requested.
// In that case the nearest possible sampling rate will be used.
func (a *Audio) Samplerate(samplerate int) (int, error) {
	return ioctl(a.file.Fd(), sndctlDspSpeed, samplerate)
}

// Channels sets the number of audio channels.
//
// The returned value may be different from the one requested.
func (a *Audio) Channels(channels int) (int, error) {
	return ioctl(a.file.Fd(), sndctlDspChannels, channels)
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
	_, err := ioctl(a.file.Fd(), sndctlDspSetfragment, arg)

	return err
}

// BufferSize returns the buffer size.
func (a *Audio) BufferSize() (int, error) {
	req := sndctlDspGetospace
	if a.mode == os.O_RDONLY {
		req = sndctlDspGetispace
	}

	info, err := ioctlI(a.file.Fd(), req)
	if err != nil {
		return 0, err
	}

	return int(info.fragstotal * info.fragsize), err
}

// RecordedBufferSize returns the number of bytes that can be read from the device.
func (a *Audio) RecordedBufferSize() (int, error) {
	info, err := ioctlI(a.file.Fd(), sndctlDspGetispace)
	if err != nil {
		return 0, err
	}

	return int(info.bytes), err
}

// UnplayedBufferSize returns the size of buffer that is not yet played.
func (a *Audio) UnplayedBufferSize() (int, error) {
	info, err := ioctlI(a.file.Fd(), sndctlDspGetospace)
	if err != nil {
		return 0, err
	}

	return int(info.fragstotal*info.fragsize - info.bytes), err
}

// Delay returns the playback buffering delay.
//
// This call tells how long it's going to take before the next sample to be written gets played by the hardware.
// The delay is returned in bytes, it can be converted to seconds by dividing it by:
//
//	samplerate*channels*sampleSizeInBytes.
func (a *Audio) Delay() (int, error) {
	return ioctl(a.file.Fd(), sndctlDspGetodelay, 0)
}

// Start starts audio playback and/or recording, depending on how the device was opened.
//
// Note that the Write and Read calls will automatically start the actual operation.
func (a *Audio) Start() error {
	var trigger int
	if a.mode != os.O_WRONLY {
		trigger |= pcmEnableInput
	}
	if a.mode != os.O_RDONLY {
		trigger |= pcmEnableOutput
	}

	_, err := ioctl(a.file.Fd(), sndctlDspSettrigger, trigger)
	if err == nil {
		a.playing = a.mode != os.O_RDONLY
		a.recording = a.mode != os.O_WRONLY
	}

	return err
}

// Write queues one audio buffer to the hardware.
func (a *Audio) Write(b []byte) (int, error) {
	if a.mode == os.O_RDONLY {
		return 0, ErrNotPlayback
	}

	n, err := a.file.Write(b)
	if err != nil {
		a.playing = false
	}

	return n, err
}

// Read reads one audio buffer from the hardware.
func (a *Audio) Read(b []byte) (int, error) {
	if a.mode == os.O_WRONLY {
		return 0, ErrNotCapture
	}

	n, err := a.file.Read(b)
	if err != nil {
		a.recording = false
	}

	return n, err
}

// Post tells the driver to start playing buffered samples immediately without
// waiting for the buffer to fill, for example after a pause or between segments.
func (a *Audio) Post() error {
	_, err := ioctl(a.file.Fd(), sndctlDspPost, 0)

	return err
}

// Reset aborts audio playback operation.
//
// There is no need to use this call after opening the device.
// The Open operation automatically initializes the device.
func (a *Audio) Reset() error {
	a.playing = false
	a.recording = false

	_, err := ioctl(a.file.Fd(), sndctlDspReset, 0)
	if err != nil {
		return err
	}

	_, err = ioctl(a.file.Fd(), sndctlDspSettrigger, 0)

	return err
}

// Sync can be used to wait until all samples written to the audio device have been played.
//
// The sync operation will always cause a more or less noticeable pause or click in the output.
// It is recommended to output few samples of silence before and after making this call.
//
// Closing the device will perform the sync operation automatically so using this ioctl call
// is unnecessary before calling Close.
func (a *Audio) Sync() error {
	_, err := ioctl(a.file.Fd(), sndctlDspSync, 0)

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
