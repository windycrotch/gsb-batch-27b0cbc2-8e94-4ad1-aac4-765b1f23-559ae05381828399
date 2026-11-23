package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSpectrumRespondsToAudio(t *testing.T) {
	silent := analyzeSpectrum(make([]float64, spectrumWindow))
	for _, v := range silent {
		if v != 0 {
			t.Fatal("silence animated the spectrum")
		}
	}
	peaks := []int{}
	for _, frequency := range []float64{120, 2000} {
		samples := make([]float64, spectrumWindow)
		for i := range samples {
			samples[i] = 0.7 * math.Sin(2*math.Pi*frequency*float64(i)/sampleRate)
		}
		levels := analyzeSpectrum(samples)
		peak := 0
		for i, v := range levels {
			if v > levels[peak] {
				peak = i
			}
		}
		if levels[peak] < 0.5 {
			t.Fatal("tone produced no significant energy")
		}
		peaks = append(peaks, peak)
	}
	if peaks[0] >= peaks[1] {
		t.Fatalf("frequency did not move spectrum peak: %v", peaks)
	}
}
func TestSpectrumStream(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := Spectrum(ctx, "../../tmp/fixtures/04-audio.wav", 2)
	f := <-ch
	if f.Err != nil {
		t.Fatal(f.Err)
	}
	if f.Time != 2 {
		t.Fatalf("seek timestamp: %v", f.Time)
	}
	energy := 0.0
	for _, v := range f.Levels {
		energy += v
	}
	if energy == 0 {
		t.Fatal("decoded tone has no spectrum")
	}
	cancel()
	for range ch {
	}
}

func TestSpectrumCoversHearingRangeWithoutTrebleGaps(t *testing.T) {
	for _, frequency := range []float64{20, 40, 120, 1000, 8000, 12345, 18500, 19900} {
		samples := make([]float64, spectrumWindow)
		for i := range samples {
			samples[i] = 0.7 * math.Sin(2*math.Pi*frequency*float64(i)/sampleRate)
		}
		levels := analyzeSpectrum(samples)
		peak := 0
		for i, v := range levels {
			if v > levels[peak] {
				peak = i
			}
		}
		if levels[peak] < 0.8 {
			t.Fatalf("%.0f Hz missing: peak %.3f", frequency, levels[peak])
		}
		// FFT resolution can spread the lowest tones across a neighboring band.
		low, high := SpectrumEdge(peak)-float64(sampleRate)/spectrumWindow, SpectrumEdge(peak+1)+float64(sampleRate)/spectrumWindow
		if frequency < low || frequency > high {
			t.Fatalf("%.0f Hz peaks in wrong band %d (%.0f–%.0f Hz)", frequency, peak, low, high)
		}
	}
}

func TestSpectrumStreamKeepsHighTrebleAndHopTiming(t *testing.T) {
	if os.Getenv("PEEP_INTEGRATION") == "" {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "treble.wav")
	if out, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=16000:sample_rate=48000:duration=1", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	ch := Spectrum(ctx, path, 0)
	for i := 0; i < 3; i++ {
		f, ok := <-ch
		if !ok || f.Err != nil {
			t.Fatalf("missing spectrum frame: %v", f.Err)
		}
		if math.Abs(f.Time-float64(i*spectrumHop)/sampleRate) > 1e-9 {
			t.Fatalf("wrong overlap timestamp: %v", f.Time)
		}
		peak := 0
		for band, level := range f.Levels {
			if level > f.Levels[peak] {
				peak = band
			}
		}
		if peak < 22 || f.Levels[peak] < 0.5 {
			t.Fatalf("16 kHz removed by analysis resampling: %v", f.Levels)
		}
	}
	cancel()
	for range ch {
	}
}
