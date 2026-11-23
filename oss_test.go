package oss_test

import (
	"bytes"
	"encoding/binary"
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

	if err := dev.Reset(); err != nil {
		t.Error(err)
	}
}

func TestCaptureWrongDirection(t *testing.T) {
	dev, err := oss.OpenCapture()
	if err != nil {
		t.Skipf("no usable capture device: %v", err)
	}
	defer dev.Close()

	if _, err := dev.Write([]byte{0, 0}); err != oss.ErrNotPlayback {
		t.Errorf("Write on capture device: got %v, want %v", err, oss.ErrNotPlayback)
	}
}
