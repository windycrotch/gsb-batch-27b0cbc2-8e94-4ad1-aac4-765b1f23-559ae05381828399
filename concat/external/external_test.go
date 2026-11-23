package external_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/concat/external"
	"github.com/SladkyCitron/gotau/umath"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/codec/wav"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test binary doubles as a fake external wavtool. Its behavior is chosen
// through an environment variable so that every path of the wrapper can be
// exercised without a real UTAU wavtool.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GOTAU_FAKE_WAVTOOL"); mode != "" {
		os.Exit(runFakeWavtool(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// A wavtool is expected to read the note file (argv[1]) and write the result
// to the output file (argv[0]).
func runFakeWavtool(mode string, args []string) int {
	if len(args) < 2 {
		return 2
	}
	output, noteInput := args[0], args[1]

	switch mode {
	case "copy":
		// Emit a valid wav file at the output path.
		f, err := os.Create(output)
		if err != nil {
			return 3
		}
		defer f.Close()
		enc, err := wav.NewEncoder(f, concatTestFormat(), afmt.SampleFormat{
			BitDepth: 16,
			Encoding: afmt.SampleEncodingInt,
			Endian:   binary.LittleEndian,
		}, wav.FormatInt)
		if err != nil {
			return 4
		}
		samples := make([]float32, 128)
		for i := range samples {
			samples[i] = 0.25
		}
		if _, err := enc.WriteSamples(samples); err != nil {
			return 5
		}
		if err := enc.Close(); err != nil {
			return 6
		}
	case "copyinput":
		data, err := os.ReadFile(noteInput)
		if err != nil {
			return 7
		}
		return writeFile(output, data)
	case "fail":
		return 1
	case "garbage":
		return writeFile(output, []byte("not a wav"))
	case "whddat":
		// Older wavtools write a .whd header plus a .dat payload instead of a
		// single wav file, and remove the wav path they were given.
		data, err := os.ReadFile(noteInput)
		if err != nil {
			return 10
		}
		stem := output[:len(output)-len(filepath.Ext(output))]
		if err := os.Remove(output); err != nil {
			return 11
		}
		split := len(data) / 2
		if code := writeFile(stem+".whd", data[:split]); code != 0 {
			return code
		}
		return writeFile(stem+".dat", data[split:])
	case "nooutput":
		// deliberately writes nothing
	default:
		return 8
	}
	return 0
}

func writeFile(path string, data []byte) int {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 9
	}
	return 0
}

func concatTestFormat() afmt.Format {
	return afmt.Format{SampleRate: 44100 * freq.Hertz, NumChannels: 1}
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

func concatTestConfig() concat.ConcatenateConfig {
	return concat.ConcatenateConfig{
		Offset:      0,
		Duration:    480,
		Tempo:       120,
		Resolution:  480,
		LengthDelta: 0,
		Overlap:     0,
		Envelope:    umathEnvelope(),
		AudioFormat: concatTestFormat(),
	}
}

func newConcatenator() *external.Concatenator {
	return external.New(os.Args[0], afmt.SampleFormat{
		BitDepth: 16,
		Encoding: afmt.SampleEncodingInt,
		Endian:   binary.LittleEndian,
	})
}

func TestConcat_SuccessCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "copy")

	out, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())
	require.NoError(t, err)

	assert.Len(t, out, 128)
	assertEmptyDir(t, tmp)
}

func TestConcat_CommandFailureCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "fail")

	_, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to run wavtool command")
	assertEmptyDir(t, tmp)
}

func TestConcat_DecodeFailureCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "garbage")

	_, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())

	require.Error(t, err)
	assertEmptyDir(t, tmp)
}

// A wavtool that writes nothing leaves the tail file in place, which is what
// UTAU-style wavtools that fail silently have always produced; either way the
// temporary directory must be cleaned up.
func TestConcat_MissingOutputCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "nooutput")

	out, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())
	if err == nil {
		assert.Empty(t, out)
	}
	assertEmptyDir(t, tmp)
}

func TestConcat_WhdDatOutputCleansUp(t *testing.T) {
	tmp := tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "whddat")

	out, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())
	require.NoError(t, err)
	assert.NotEmpty(t, out, "whd + dat output should decode to samples")
	assertEmptyDir(t, tmp)
}

func TestConcat_ConcurrentCallsDoNotInterfere(t *testing.T) {
	tempDir(t)
	t.Setenv("GOTAU_FAKE_WAVTOOL", "copyinput")

	reference, err := newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())
	require.NoError(t, err)

	const workers = 6
	results := make([][]float32, workers)
	errs := make([]error, workers)
	done := make(chan int, workers)
	for i := range workers {
		go func() {
			results[i], errs[i] = newConcatenator().Concatenate(nil, make([]float32, 100), concatTestConfig())
			done <- i
		}()
	}
	for range workers {
		<-done
	}
	for i := range workers {
		require.NoError(t, errs[i])
		assert.Equal(t, reference, results[i], "worker %d got a different result", i)
	}
}

func umathEnvelope() umath.Envelope {
	return umath.Envelope{
		{X: 0, Y: 0},
		{X: 0, Y: 1},
		{X: 0, Y: 1},
		{X: 0, Y: 0},
	}
}
