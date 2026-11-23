// Package external implements an external resampler.
package external

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SladkyCitron/gotau/pitch"
	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
)

var _ resampler.Analyzer = (*Resampler)(nil)

// Resampler is a resampler that uses an external command-line UTAU resampler program to perform resampling.
type Resampler struct {
	// ConfigureCmd is an optional hook that allows configuring the exec.Cmd before running it.
	ConfigureCmd func(cmd *exec.Cmd)

	cmdName     string
	sampleFmt   afmt.SampleFormat
	analysisExt string
}

// New creates a new [Resampler] with the given program name and
// sample format for encoding temporary WAV files for passing into the resampler.
//
// The program should be a command-line UTAU resampler (e.g. resampler, moresampler, straycat, etc.)
// that accepts input and output WAV file paths, analysis sidecar files (optional), and
// other parameters as arguments and processes the input WAV file accordingly.
func New(name string, analysisExt string, sampleFmt afmt.SampleFormat) *Resampler {
	return &Resampler{cmdName: name, sampleFmt: sampleFmt, analysisExt: analysisExt}
}

func (r *Resampler) ID() string {
	return fmt.Sprintf("external:%s:%s", r.cmdName, r.analysisExt)
}

func (r *Resampler) Resample(in aio.SampleReader, cfg resampler.ResampleConfig) (aio.SampleReader, error) {
	return r.resample(in, nil, cfg)
}

func (r *Resampler) ResampleWithAnalysis(in aio.SampleReader, analysis io.Reader, cfg resampler.ResampleConfig) (aio.SampleReader, error) {
	if analysis == nil {
		return r.Resample(in, cfg)
	}
	return r.resample(in, analysis, cfg)
}

// resample runs the external resampler using a private temporary directory.
//
// A dedicated directory per invocation keeps concurrent resampler runs (for the
// same note or not) from overwriting each other's temporary WAV and analysis
// files. The directory is removed on every path out of this function, so a
// failed command or a failed read never leaves temporary or output files
// behind. The returned reader decodes from memory, so no files outlive it.
func (r *Resampler) resample(in aio.SampleReader, analysis io.Reader, cfg resampler.ResampleConfig) (aio.SampleReader, error) {
	dir, err := os.MkdirTemp("", "gotau-externalresampler-*")
	if err != nil {
		return nil, fmt.Errorf("external: failed to create temporary directory: %w", err)
	}
	// Whether we succeed or fail, nothing in the directory is needed after this
	// function returns; the decoded samples live in memory.
	defer func() { _ = os.RemoveAll(dir) }()

	input := filepath.Join(dir, "in.wav")
	output := filepath.Join(dir, "out.wav")

	if err := r.createTempWav(input, in, cfg.AudioFormat); err != nil {
		return nil, fmt.Errorf("external: failed to create temporary wav file: %w", err)
	}

	if analysis != nil {
		if err := r.createTempAnalysis(analysis, input); err != nil {
			return nil, fmt.Errorf("external: failed to create temporary analysis sidecar file: %w", err)
		}
	}

	if err := r.runCmd(input, output, cfg); err != nil {
		return nil, err
	}

	out, err := r.decodeOutFile(output)
	if err != nil {
		return nil, fmt.Errorf("external: failed to decode output wav file: %w", err)
	}

	return out, nil
}

func (r *Resampler) Analyze(in aio.SampleReader, format afmt.Format) (io.ReadCloser, error) {
	dir, err := os.MkdirTemp("", "gotau-externalresampler-analysis-*")
	if err != nil {
		return nil, fmt.Errorf("external: failed to create temporary directory: %w", err)
	}

	input := filepath.Join(dir, "in.wav")
	output := filepath.Join(dir, "out.wav")

	// The analysis file itself must outlive this call; the caller owns it and
	// removes it on Close. Everything else is cleaned up here.
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()

	if err := r.createTempWav(input, in, format); err != nil {
		return nil, fmt.Errorf("external: failed to create temporary wav file: %w", err)
	}

	if err := r.runAnalyzeCmd(input, output); err != nil {
		return nil, err
	}

	// most resamplers accept e.g. something_wav.frq
	analysisPath := analysisSidecarPath(input, r.analysisExt)
	rc, err := openTemp(analysisPath)
	if err != nil {
		return nil, fmt.Errorf("external: failed to open analysis sidecar file: %w", err)
	}

	// Remove the analysis file along with the rest of the directory when the
	// caller closes the returned reader.
	keep = true
	return &dirCleanupReadCloser{ReadCloser: rc, dir: dir}, nil
}

func (r *Resampler) AnalysisExt() string {
	return r.analysisExt
}

func (r *Resampler) runCmd(input, output string, cfg resampler.ResampleConfig) error {
	flags := "?"
	if len(cfg.Flags) > 0 {
		flags = cfg.Flags.String()
	}

	cmd := exec.Command(
		r.cmdName,
		input,
		output,
		pitch.FormatNote(cfg.Pitch, 's'),
		strconv.FormatInt(int64(cfg.Velocity), 10),
		flags,
		strconv.FormatFloat(cfg.Offset, 'f', -1, 64),
		strconv.FormatFloat(cfg.Length, 'f', -1, 64),
		strconv.FormatFloat(cfg.Consonant, 'f', -1, 64),
		strconv.FormatFloat(cfg.Cutoff, 'f', -1, 64),
		strconv.FormatInt(int64(cfg.Intensity*100), 10),
		strconv.FormatInt(int64(cfg.Modulation*100), 10),
		"!"+strconv.FormatFloat(cfg.Tempo, 'f', -1, 64), // apparently the tempo starts with "!" and not "T"???
		pitch.EncodeResamplerPitchBendString(cfg.PitchBend),
	)
	if r.ConfigureCmd != nil {
		r.ConfigureCmd(cmd)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("external: failed to run resampler command: %w", err)
	}

	return nil
}

func (r *Resampler) runAnalyzeCmd(input, output string) error {
	cmd := exec.Command(r.cmdName, input, output, "0", "0", "GN")
	if r.ConfigureCmd != nil {
		r.ConfigureCmd(cmd)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("external: failed to run resampler command: %w", err)
	}
	return nil
}

// analysisSidecarPath returns the UTAU-style analysis sidecar path for the given
// wav path, e.g. "song.wav" -> "song_wav.frq".
func analysisSidecarPath(wavPath, analysisExt string) string {
	ext := filepath.Ext(wavPath)
	extless := wavPath[:len(wavPath)-len(ext)]
	return extless + strings.ReplaceAll(ext, ".", "_") + analysisExt
}

func (r *Resampler) createTempWav(path string, in aio.SampleReader, format afmt.Format) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	var wavFormat uint16
	switch r.sampleFmt.Encoding {
	case afmt.SampleEncodingInt, afmt.SampleEncodingUint:
		wavFormat = wav.FormatInt
	case afmt.SampleEncodingFloat:
		wavFormat = wav.FormatFloat
	default:
		return fmt.Errorf("invalid sample format: %s", r.sampleFmt.String())
	}
	enc, err := wav.NewEncoder(f, format, r.sampleFmt, wavFormat)
	if err != nil {
		return err
	}

	if _, err := aio.Copy(enc, in); err != nil {
		return err
	}

	return enc.Close()
}

func (r *Resampler) createTempAnalysis(analysis io.Reader, wavPath string) (err error) {
	f, err := os.Create(analysisSidecarPath(wavPath, r.analysisExt))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	if _, err := io.Copy(f, analysis); err != nil {
		return err
	}

	return nil
}

func (r *Resampler) decodeOutFile(path string) (aio.SampleReader, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	deco, err := wav.NewDecoder(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	return deco, nil
}

// dirCleanupReadCloser removes a temporary directory when the wrapped reader is
// closed.
type dirCleanupReadCloser struct {
	io.ReadCloser
	dir string
}

func (c *dirCleanupReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if rmErr := os.RemoveAll(c.dir); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}
