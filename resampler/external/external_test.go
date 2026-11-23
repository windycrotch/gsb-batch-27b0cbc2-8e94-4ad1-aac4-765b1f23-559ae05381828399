package external_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/gotau/resampler/external"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test binary doubles as a fake external resampler. Its behavior is chosen
// through environment variables so that every path of the wrapper can be
// exercised without depending on a real UTAU resampler.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GOTAU_FAKE_RESAMPLER"); mode != "" {
		os.Exit(runFakeResampler(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakeResampler(mode string, args []string) int {
	if len(args) < 2 {
		return 2
	}
	input, output := args[0], args[1]

	// Analyze invokes the command as: input output 0 0 GN
	if len(args) == 5 && args[2] == "0" && args[3] == "0" && args[4] == "GN" {
		sidecar := input[:len(input)-len(filepath.Ext(input))]
		sidecar += "_wav.frq"
		if err := os.WriteFile(sidecar, []byte("analysis"), 0o644); err != nil {
			return 3
		}
		return 0
	}

	switch mode {
	case "copy":
		data, err := os.ReadFile(input)
		if err != nil {
			return 4
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return 5
		}
	case "fail":
		return 1
	case "garbage":
		if err := os.WriteFile(output, []byte("not a wav file"), 0o644); err != nil {
			return 6
		}
	case "nooutput":
		// deliberately writes nothing
	default:
		return 7
	}
	return 0
}

func testFormat() afmt.Format {
	return afmt.Format{SampleRate: 44100 * freq.Hertz, NumChannels: 1}
}

func testConfig() resampler.ResampleConfig {
	return resampler.ResampleConfig{
		Pitch:       60,
		Velocity:    100,
		Offset:      18,
		Length:      250,
		Consonant:   80,
		Cutoff:      49,
		Intensity:   1,
		Tempo:       120,
		AudioFormat: testFormat(),
	}
}

func newResampler() *external.Resampler {
	return external.New(os.Args[0], ".frq", afmt.SampleFormat{
		BitDepth: 16,
		Encoding: afmt.SampleEncodingInt,
		Endian:   binary.LittleEndian,
	})
}

// tempDir pins the process temp directory to a fresh directory so that the
// wrapper's temporary files can be inspected without interference.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Empty(t, names, "temporary files must be cleaned up")
}

func inputReader() aio.SampleReader {
	samples := make([]float32, 1000)
	for i := range samples {
		samples[i] = float32(i) / 1000
	}
	return &sliceReader{samples: samples}
}

func TestResample_SuccessCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "copy")

	out, err := newResampler().Resample(inputReader(), testConfig())
	require.NoError(t, err)
	got, err := aio.ReadAll(out)
	require.NoError(t, err)

	assert.Len(t, got, 1000, "the fake resampler echoes its input")
	// The 16-bit temporary WAV quantizes the samples.
	assert.InDelta(t, 0.5, got[500], 1.0/32768)
	assertEmptyDir(t, tmp)
}

func TestResample_CommandFailureCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "fail")

	_, err := newResampler().Resample(inputReader(), testConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to run resampler command")
	assertEmptyDir(t, tmp)
}

func TestResample_DecodeFailureCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "garbage")

	_, err := newResampler().Resample(inputReader(), testConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decode output wav file")
	assertEmptyDir(t, tmp)
}

func TestResample_MissingOutputCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "nooutput")

	_, err := newResampler().Resample(inputReader(), testConfig())

	require.Error(t, err)
	assertEmptyDir(t, tmp)
}

func TestResampleWithAnalysis_CleansUpSidecar(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "copy")

	out, err := newResampler().ResampleWithAnalysis(inputReader(), bytes.NewReader([]byte("sidecar")), testConfig())
	require.NoError(t, err)
	_, err = aio.ReadAll(out)
	require.NoError(t, err)

	assertEmptyDir(t, tmp)
}

func TestResample_ConcurrentCallsDoNotInterfere(t *testing.T) {
	tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "copy")

	const workers = 6
	done := make(chan error, workers)
	for range workers {
		go func() {
			out, err := newResampler().Resample(inputReader(), testConfig())
			if err == nil {
				_, err = aio.ReadAll(out)
			}
			done <- err
		}()
	}
	for range workers {
		require.NoError(t, <-done)
	}
}

func TestAnalyze_SidecarRemovedOnClose(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_RESAMPLER", "copy")

	rc, err := newResampler().Analyze(inputReader(), testFormat())
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "analysis", string(data))

	// The sidecar is intentionally kept until the caller is done with it.
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	require.NoError(t, rc.Close())
	assertEmptyDir(t, tmp)
}

type sliceReader struct {
	samples []float32
	pos     int
}

func (r *sliceReader) ReadSamples(p []float32) (int, error) {
	if r.pos >= len(r.samples) {
		return 0, io.EOF
	}
	n := copy(p, r.samples[r.pos:])
	r.pos += n
	if r.pos >= len(r.samples) {
		return n, io.EOF
	}
	return n, nil
}
