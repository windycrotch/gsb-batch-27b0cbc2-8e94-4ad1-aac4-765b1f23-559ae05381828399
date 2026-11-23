package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
)

const SpectrumBands = 24
const SpectrumMinHz = 20.0
const SpectrumMaxHz = 20000.0
const sampleRate = 48000

// Overlapping windows resolve sub-bass while updating about 23 times/second.
const spectrumWindow = 8192
const spectrumHop = 2048

type SpectrumFrame struct {
	Time   float64
	Levels [SpectrumBands]float64
	Err    error
}

// Spectrum analyzes actual mono PCM in log-spaced bands, with bounded prefetch.
// Its timestamps share the media clock used by mpv and are reset on seeks.
func Spectrum(ctx context.Context, path string, start float64) <-chan SpectrumFrame {
	out := make(chan SpectrumFrame, 2)
	go func() {
		defer close(out)
		send := func(f SpectrumFrame) bool {
			select {
			case out <- f:
				return true
			case <-ctx.Done():
				return false
			}
		}
		args := []string{"-v", "error", "-nostdin", "-threads", "1"}
		if start > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", start))
		}
		args = append(args, "-i", path, "-vn", "-sn", "-ac", "1", "-ar", strconv.Itoa(sampleRate), "-f", "s16le", "pipe:1")
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			send(SpectrumFrame{Err: err})
			return
		}
		if err = cmd.Start(); err != nil {
			send(SpectrumFrame{Err: err})
			return
		}
		buf := make([]byte, spectrumWindow*2)
		samples := make([]float64, spectrumWindow)
		count, size := 0, spectrumWindow
		for {
			n, readErr := io.ReadFull(pipe, buf[:size*2])
			if n == 0 {
				break
			}
			if size == spectrumHop {
				copy(samples, samples[spectrumHop:])
			}
			for i := 0; i < size; i++ {
				value := 0.0
				if 2*i+1 < n {
					value = float64(int16(binary.LittleEndian.Uint16(buf[i*2:]))) / 32768
				}
				samples[spectrumWindow-size+i] = value
			}
			f := SpectrumFrame{Time: start + float64(count*spectrumHop)/sampleRate, Levels: analyzeSpectrum(samples)}
			if !send(f) {
				break
			}
			count++
			size = spectrumHop
			if readErr != nil {
				break
			}
		}
		err = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			send(SpectrumFrame{Err: fmt.Errorf("audio spectrum: %w", err)})
		}
	}()
	return out
}

// SpectrumEdge returns the logarithmic boundary of a band, including both ends.
func SpectrumEdge(edge int) float64 {
	return SpectrumMinHz * math.Pow(SpectrumMaxHz/SpectrumMinHz, float64(edge)/SpectrumBands)
}

func analyzeSpectrum(samples []float64) (levels [SpectrumBands]float64) {
	n := len(samples)
	if n < 2 || n&(n-1) != 0 {
		return
	}
	bins := make([]complex128, n)
	windowPower := 0.0
	for i, v := range samples {
		window := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		bins[i] = complex(v*window, 0)
		windowPower += window * window
	}
	// In-place radix-2 FFT. Integrating bins across each band avoids gaps
	// between the widely spaced treble frequencies of point sampling.
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			bins[i], bins[j] = bins[j], bins[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		angle := -2 * math.Pi / float64(size)
		step := complex(math.Cos(angle), math.Sin(angle))
		for start := 0; start < n; start += size {
			phase := complex(1.0, 0)
			for j := 0; j < size/2; j++ {
				even, odd := bins[start+j], phase*bins[start+j+size/2]
				bins[start+j], bins[start+j+size/2] = even+odd, even-odd
				phase *= step
			}
		}
	}
	for band := range levels {
		first := max(1, int(math.Ceil(SpectrumEdge(band)*float64(n)/sampleRate)))
		end := min(n/2+1, int(math.Ceil(SpectrumEdge(band+1)*float64(n)/sampleRate)))
		power := 0.0
		for k := first; k < end; k++ {
			power += real(bins[k])*real(bins[k]) + imag(bins[k])*imag(bins[k])
		}
		amplitude := math.Sqrt(4 * power / (float64(n) * windowPower))
		db := 20 * math.Log10(max(amplitude, 1e-9))
		levels[band] = max(0, min(1, (db+60)/60))
	}
	return
}
