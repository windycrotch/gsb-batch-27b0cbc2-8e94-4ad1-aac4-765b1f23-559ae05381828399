package oss_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gen2brain/oss"
)

func TestAudio(t *testing.T) {
	dev, err := oss.OpenAudio()
	if err != nil {
		t.Skipf("no usable audio device: %v", err)
	}
	defer dev.Close()

	formats, err := dev.Formats()
	if err != nil {
		t.Skipf("audio device does not support PCM setup: %v", err)
	}

	err = dev.SetBufferSize(2048, 2)
	if err != nil {
		t.Error(err)
	}

	caps, err := dev.Caps()
	if err != nil {
		t.Error(err)
	}

	if caps&int(oss.DspCapTrigger) == 0 {
		t.Errorf("Controls: no trigger cap")
	}

	ok, err := dev.IsCap(oss.DspCapTrigger)
	if err != nil {
		t.Error(err)
	}

	if !ok {
		t.Errorf("IsCap: got %v, want %v", ok, true)
	}

	if formats&int(oss.AfmtS16Le) == 0 {
		t.Errorf("Formats: no %s format", oss.AfmtS16Le)
	}

	ok, err = dev.IsFormat(oss.AfmtS16Le)
	if err != nil {
		t.Error(err)
	}

	if !ok {
		t.Errorf("IsFormat: got %v, want %v", ok, true)
	}

	channels, err := dev.Channels(2)
	if err != nil {
		t.Error(err)
	}

	if channels != 2 {
		t.Errorf("Channels: got %d, want %d", channels, 2)
	}

	speed := 44100
	samplerate, err := dev.Samplerate(speed)
	if err != nil {
		t.Error(err)
	}

	if samplerate != speed {
		t.Errorf("Samplerate: got %d, want %d", samplerate, speed)
	}

	reqFormat := oss.AfmtS16Le
	format, err := dev.Format(reqFormat)
	if err != nil {
		t.Error(err)
	}

	if format != reqFormat {
		t.Errorf("Format: got %d, want %d", format, reqFormat)
	}

	err = dev.Start()
	if err != nil {
		t.Error(err)
	}

	// Play 1 second of beep.
	duration := 1 * time.Second
	for x := 0.; x < duration.Seconds(); {
		var buf bytes.Buffer

		for i := 0; i < 2048; i++ {
			v := math.Sin(x * 2 * math.Pi * 440)
			v *= 0.1 // make it a little quieter

			sample := int16(v * math.MaxInt16)

			for c := 0; c < channels; c++ {
				err = binary.Write(&buf, binary.LittleEndian, sample)
				if err != nil {
					t.Error(err)
				}
			}

			x += 1 / float64(samplerate)
		}

		_, err := dev.Write(buf.Bytes())
		if err != nil {
			t.Error(err)
		}
	}

	playing := dev.Playing()
	if !playing {
		t.Errorf("Playing: not playing")
	}

	_, err = dev.BufferSize()
	if err != nil {
		t.Error(err)
	}

	_, err = dev.Delay()
	if err != nil {
		t.Error(err)
	}

	_, err = dev.UnplayedBufferSize()
	if err != nil {
		t.Error(err)
	}

	err = dev.Sync()
	if err != nil {
		t.Error(err)
	}

	err = dev.Reset()
	if err != nil {
		t.Error(err)
	}

	if dev.Playing() {
		t.Error("Playing after Reset = true, want false")
	}
	if dev.Recording() {
		t.Error("Recording after Reset = true, want false")
	}

	// Writing after Reset must not report the device as playing again.
	if _, err := dev.Write(make([]byte, 64)); err != nil {
		t.Errorf("Write after Reset: %v", err)
	}
	if dev.Playing() {
		t.Error("Playing after Write following Reset = true, want false")
	}
}

func TestCapture(t *testing.T) {
	dev, err := oss.OpenCapture()
	if err != nil {
		t.Skipf("no usable capture device: %v", err)
	}
	defer dev.Close()

	if _, err := dev.Formats(); err != nil {
		t.Skipf("capture device does not support PCM setup: %v", err)
	}

	format, err := dev.Format(oss.AfmtS16Le)
	if err != nil {
		t.Skipf("capture device does not support S16Le: %v", err)
	}

	if format != oss.AfmtS16Le {
		t.Errorf("Format: got %s, want %s", format, oss.AfmtS16Le)
	}

	channels, err := dev.Channels(2)
	if err != nil {
		t.Error(err)
	}

	speed := 44100
	samplerate, err := dev.Samplerate(speed)
	if err != nil {
		t.Error(err)
	}

	// Record a short buffer and make sure the requested amount is read back.
	// Reading starts the capture automatically, no explicit Start is needed.
	buf := make([]byte, channels*2*samplerate/10)

	total := 0
	for total < len(buf) {
		n, err := dev.Read(buf[total:])
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n == 0 {
			t.Fatal("Read: returned 0 bytes")
		}

		total += n
	}

	if total != len(buf) {
		t.Errorf("Read: got %d bytes, want %d", total, len(buf))
	}

	if _, err := dev.RecordedBufferSize(); err != nil {
		t.Error(err)
	}

	if _, err := dev.BufferSize(); err != nil {
		t.Error(err)
	}

	if _, err := dev.UnplayedBufferSize(); !errors.Is(err, oss.ErrNotPlayback) {
		t.Errorf("UnplayedBufferSize on capture: got %v, want ErrNotPlayback", err)
	}

	if err := dev.Reset(); err != nil {
		t.Error(err)
	}

	if dev.Recording() {
		t.Error("Recording after Reset = true, want false")
	}
}

func TestPlaybackUnplayedBufferSize(t *testing.T) {
	dev, err := oss.OpenAudio()
	if err != nil {
		t.Skipf("no usable audio device: %v", err)
	}
	defer dev.Close()

	if _, err := dev.Formats(); err != nil {
		t.Skipf("audio device does not support PCM setup: %v", err)
	}

	if _, err := dev.UnplayedBufferSize(); err != nil {
		t.Errorf("UnplayedBufferSize on playback: %v", err)
	}
}

func TestCaptureWrongDirection(t *testing.T) {
	dev, err := oss.OpenCapture()
	if err != nil {
		t.Skipf("no usable capture device: %v", err)
	}
	defer dev.Close()

	if _, err := dev.Write([]byte{0, 0}); !errors.Is(err, oss.ErrNotPlayback) {
		t.Errorf("Write on capture device: got %v, want %v", err, oss.ErrNotPlayback)
	}
}

func TestPlaybackWrongDirection(t *testing.T) {
	dev, err := oss.OpenAudio()
	if err != nil {
		t.Skipf("no usable audio device: %v", err)
	}
	defer dev.Close()

	if _, err := dev.Read(make([]byte, 2)); !errors.Is(err, oss.ErrNotCapture) {
		t.Errorf("Read on playback device: got %v, want %v", err, oss.ErrNotCapture)
	}
}

func TestClosedDevice(t *testing.T) {
	dev, err := oss.OpenAudio()
	if err != nil {
		t.Skipf("no usable audio device: %v", err)
	}

	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if dev.Playing() || dev.Recording() {
		t.Error("closed device reports playing/recording, want false/false")
	}

	checks := map[string]error{
		"Caps":               errOf(dev.Caps),
		"Formats":            errOf(dev.Formats),
		"Start":              dev.Start(),
		"Reset":              dev.Reset(),
		"Post":               dev.Post(),
		"Sync":               dev.Sync(),
		"SetBufferSize":      dev.SetBufferSize(2048, 2),
		"BufferSize":         errOf(dev.BufferSize),
		"RecordedBufferSize": errOf(dev.RecordedBufferSize),
		"UnplayedBufferSize": errOf(dev.UnplayedBufferSize),
		"Write":              errOfWrite(dev.Write),
		"Read":               errOfRead(dev.Read),
	}

	for name, err := range checks {
		if !errors.Is(err, oss.ErrClosed) {
			t.Errorf("%s after Close: got %v, want ErrClosed", name, err)
		}
	}

	// A second Close is harmless and recognizable.
	if err := dev.Close(); !errors.Is(err, oss.ErrClosed) {
		t.Errorf("second Close: got %v, want ErrClosed", err)
	}
}

func errOf(f func() (int, error)) error            { _, err := f(); return err }
func errOfWrite(f func([]byte) (int, error)) error { _, err := f([]byte{0, 0}); return err }
func errOfRead(f func([]byte) (int, error)) error  { _, err := f(make([]byte, 2)); return err }
