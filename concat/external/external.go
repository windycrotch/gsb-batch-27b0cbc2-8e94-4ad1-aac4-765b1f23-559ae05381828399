// Package external implements an external concatenator.
package external

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/internal/timeutil"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
)

var _ concat.Concatenator = (*Concatenator)(nil)

// Concatenator is a concatenator that uses an external command-line
// UTAU wavtool program to perform concatenation.
type Concatenator struct {
	// ConfigureCmd is an optional hook that allows configuring the
	// exec.Cmd before running it.
	ConfigureCmd func(cmd *exec.Cmd)

	cmdName   string
	sampleFmt afmt.SampleFormat
}

// New creates a new [Concatenator] with the given program name and
// sample format for encoding temporary WAV files for passing into the concatenator.
//
// The program should be a command-line UTAU wavtool (e.g. wavtool, wavtool-yawu, etc.)
// that accepts tail and note WAV file paths and
// other parameters as arguments and concatenates accordingly.
func New(name string, sampleFmt afmt.SampleFormat) *Concatenator {
	return &Concatenator{cmdName: name, sampleFmt: sampleFmt}
}

func (c *Concatenator) Concatenate(tail []float32, note []float32, cfg concat.ConcatenateConfig) ([]float32, error) {
	if len(cfg.Envelope) < 4 || len(cfg.Envelope) > 5 {
		return nil, fmt.Errorf("envelope curve must have 4 or 5 points, got %d", len(cfg.Envelope))
	}

	// A private temporary directory per invocation keeps concurrent renders
	// from overwriting each other's tail/note/output files, and lets every exit
	// path clean up reliably: nothing in it is needed once the samples have been
	// decoded into memory.
	dir, err := os.MkdirTemp("", "gotau-externalconcat-*")
	if err != nil {
		return nil, fmt.Errorf("external: failed to create temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tailWav := filepath.Join(dir, "tail.wav")
	noteWav := filepath.Join(dir, "note.wav")

	if err := c.createTempWav(tailWav, noteWav, tail, note, cfg); err != nil {
		return nil, fmt.Errorf("external: failed to create temporary wav files: %w", err)
	}

	var length string
	// most wavtools have TPQN 480 hardcoded so if our TPQN is 480 we use the wavtool length syntax
	// and if it's anything other than 480 then we calculate the length here instead
	if cfg.Resolution == 480 {
		// ticks@bpm+-correction
		length = fmt.Sprintf("%d@%.0f%+.0f", cfg.Duration, cfg.Tempo, cfg.LengthDelta)
	} else {
		length = strconv.FormatFloat((timeutil.TicksToSeconds(cfg.Duration, cfg.Resolution, cfg.Tempo)*1000)+cfg.LengthDelta, 'f', -1, 64)
	}

	args := []string{
		tailWav,                                  // output file
		noteWav,                                  // input file
		strconv.FormatInt(int64(cfg.Offset), 10), // STP
		length,                                   // note length
		strconv.FormatInt(int64(cfg.Envelope[0].X), 10),     // p1
		strconv.FormatInt(int64(cfg.Envelope[1].X), 10),     // p2
		strconv.FormatInt(int64(cfg.Envelope[2].X), 10),     // p3
		strconv.FormatInt(int64(cfg.Envelope[0].Y*100), 10), // v1
		strconv.FormatInt(int64(cfg.Envelope[1].Y*100), 10), // v2
		strconv.FormatInt(int64(cfg.Envelope[2].Y*100), 10), // v3
		strconv.FormatInt(int64(cfg.Envelope[3].Y*100), 10), // v4
		strconv.FormatInt(int64(cfg.Overlap), 10),           // overlap
		strconv.FormatInt(int64(cfg.Envelope[3].X), 10),     // p4
	}
	if len(cfg.Envelope) >= 5 {
		args = append(
			args,
			strconv.FormatInt(int64(cfg.Envelope[4].X), 10),     // p5
			strconv.FormatInt(int64(cfg.Envelope[4].Y*100), 10), // v5
		)
	}
	cmd := exec.Command(c.cmdName, args...)
	if c.ConfigureCmd != nil {
		c.ConfigureCmd(cmd)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("external: failed to run wavtool command: %w", err)
	}

	// The wavtool may write a single wav file or a .whd + .dat pair; the
	// temporary directory is discarded afterwards either way.
	//
	// A stat error other than "does not exist" (e.g. permission denied) is not
	// treated as "no wav output", so that it surfaces as a decode error instead
	// of silently switching to the whd/dat path.
	if _, statErr := os.Stat(tailWav); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
		out, err := c.decodeWavFile(tailWav)
		if err != nil {
			return nil, fmt.Errorf("external: failed to decode temporary output wav: %w", err)
		}
		return out, nil
	}

	whdPath := tailWav[:len(tailWav)-len(filepath.Ext(tailWav))] + ".whd"
	datPath := tailWav[:len(tailWav)-len(filepath.Ext(tailWav))] + ".dat"

	whdFile, err := os.Open(whdPath)
	if err != nil {
		return nil, fmt.Errorf("external: failed to open temporary whd: %w", err)
	}
	defer whdFile.Close()

	datFile, err := os.Open(datPath)
	if err != nil {
		return nil, fmt.Errorf("external: failed to open temporary dat: %w", err)
	}
	defer datFile.Close()

	deco, err := wav.NewDecoder(io.MultiReader(whdFile, datFile))
	if err != nil {
		return nil, fmt.Errorf("external: failed to decode output wav: %w", err)
	}
	out, err := aio.ReadAll(deco)
	if err != nil {
		return nil, fmt.Errorf("external: failed to read output wav: %w", err)
	}
	return out, nil
}

func (c *Concatenator) decodeWavFile(path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	deco, err := wav.NewDecoder(f)
	if err != nil {
		return nil, err
	}
	return aio.ReadAll(deco)
}

func (c *Concatenator) createTempWav(tailPath, notePath string, tail []float32, note []float32, cfg concat.ConcatenateConfig) error {
	var wavFormat uint16
	switch c.sampleFmt.Encoding {
	case afmt.SampleEncodingInt, afmt.SampleEncodingUint:
		wavFormat = wav.FormatInt
	case afmt.SampleEncodingFloat:
		wavFormat = wav.FormatFloat
	default:
		return fmt.Errorf("invalid sample format: %s", c.sampleFmt.String())
	}

	if err := writeWav(tailPath, tail, cfg.AudioFormat, c.sampleFmt, wavFormat); err != nil {
		return err
	}
	return writeWav(notePath, note, cfg.AudioFormat, c.sampleFmt, wavFormat)
}

func writeWav(path string, samples []float32, format afmt.Format, sampleFmt afmt.SampleFormat, wavFormat uint16) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	enc, err := wav.NewEncoder(f, format, sampleFmt, wavFormat)
	if err != nil {
		return err
	}
	if _, err := enc.WriteSamples(samples); err != nil {
		return err
	}
	return enc.Close()
}
