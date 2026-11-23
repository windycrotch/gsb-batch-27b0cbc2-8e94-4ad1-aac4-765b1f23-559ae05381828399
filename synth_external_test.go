package gotau_test

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/SladkyCitron/gotau"
	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/diskcache"
	concatExternal "github.com/SladkyCitron/gotau/concat/external"
	"github.com/SladkyCitron/gotau/resampler/external"
	"github.com/SladkyCitron/gotau/sequence"
	"github.com/SladkyCitron/gotau/umath"
	"github.com/SladkyCitron/gotau/voicebank"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lets the test binary act as a fake external resampler or wavtool so
// that the external engine path can be exercised end to end, without depending
// on a real UTAU resampler or wavtool being installed.
func TestMain(m *testing.M) {
	if os.Getenv("GOTAU_FAKE_RESAMPLER") != "" || os.Getenv("GOTAU_FAKE_WAVTOOL") != "" {
		os.Exit(runEngineFake(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runEngineFake distinguishes the two roles by their arguments: a resampler
// receives the target pitch as argv[2] (e.g. "C4"), while a wavtool receives a
// numeric STP offset there.
func runEngineFake(args []string) int {
	if len(args) < 2 {
		return 2
	}

	// Analyze is invoked as: input output 0 0 GN
	if len(args) == 5 && args[2] == "0" && args[3] == "0" && args[4] == "GN" {
		input := args[0]
		sidecar := input[:len(input)-len(filepath.Ext(input))] + "_wav.frq"
		if err := os.WriteFile(sidecar, []byte("analysis"), 0o644); err != nil {
			return 3
		}
		return 0
	}

	// A resampler receives the target pitch as argv[2] (e.g. "C5"); a wavtool
	// receives a numeric STP offset there and takes (output, note input).
	if _, err := strconv.ParseFloat(args[2], 64); err == nil {
		return runFakeWavtool(os.Getenv("GOTAU_FAKE_WAVTOOL"), args[0], args[1])
	}
	return runFakeResampler(os.Getenv("GOTAU_FAKE_RESAMPLER"), args[0], args[1])
}

func runFakeResampler(mode, input, output string) int {
	switch mode {
	case "copy":
		data, err := os.ReadFile(input)
		if err != nil {
			return 4
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return 5
		}
		return 0
	case "fail":
		return 1
	default:
		return 6
	}
}

// The wavtool reads the note file and writes the result to the output file
// (which initially contains the tail).
func runFakeWavtool(mode, output, noteInput string) int {
	switch mode {
	case "copy":
		data, err := os.ReadFile(noteInput)
		if err != nil {
			return 7
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return 8
		}
		return 0
	case "fail":
		return 1
	default:
		return 9
	}
}

// newOnDiskVoicebank writes a complete voicebank to a temporary directory so
// that external commands can read real files from it.
func newOnDiskVoicebank(t *testing.T) *voicebank.Voicebank {
	t.Helper()
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "oto.ini"), []byte("sample.wav=a,0,120,-39,80,20\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sample.wav"), encodeTestWav(t, make([]float32, testSampleRate/10)), 0o644))

	vb, err := voicebank.Open(os.DirFS(dir))
	require.NoError(t, err)
	return vb
}

func integrationSequence() sequence.Sequence {
	return sequence.Sequence{
		Metadata: sequence.Metadata{Resolution: 480, Tempo: 120},
		Notes: []sequence.Note{{
			Position: 0,
			Duration: 480,
			Lyric:    "a",
			Note:     60,
			Velocity: 100,
			Envelope: umath.Envelope{
				{X: 0, Y: 0},
				{X: 0, Y: 1},
				{X: 100, Y: 1},
				{X: 200, Y: 0},
			},
		}},
	}
}

func integrationSynth(t *testing.T, resamplerMode, wavtoolMode string) *gotau.Synth {
	t.Helper()
	return integrationSynthWithCache(t, newOnDiskVoicebank(t), resamplerMode, wavtoolMode, &cache.NopCache{})
}

func integrationSynthWithCache(t *testing.T, vb *voicebank.Voicebank, resamplerMode, wavtoolMode string, c cache.Cache) *gotau.Synth {
	t.Helper()
	setFakeEnv(t, resamplerMode, wavtoolMode)
	return newIntegrationSynth(t, vb, c)
}

// setFakeEnv selects the fake command behaviors. It must not be called from
// goroutines other than the test goroutine.
func setFakeEnv(t *testing.T, resamplerMode, wavtoolMode string) {
	t.Helper()
	t.Setenv("GOTAU_FAKE_RESAMPLER", resamplerMode)
	t.Setenv("GOTAU_FAKE_WAVTOOL", wavtoolMode)
}

// newIntegrationSynth builds a synth using whatever fake command behaviors are
// already in the environment. It is safe to call from goroutines.
func newIntegrationSynth(t *testing.T, vb *voicebank.Voicebank, c cache.Cache) *gotau.Synth {
	t.Helper()
	sampleFmt := afmt.SampleFormat{BitDepth: 16, Encoding: afmt.SampleEncodingInt, Endian: binary.LittleEndian}
	res := external.New(os.Args[0], ".frq", sampleFmt)
	cat := concatExternal.New(os.Args[0], sampleFmt)
	s := gotau.New(testSampleRate, vb, res, cat)
	s.SetResamplerCache(c)
	return s
}

func pinTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func assertTempEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Empty(t, names, "temporary files must be cleaned up")
}

func TestIntegrationExternalSuccess(t *testing.T) {
	tmp := pinTempDir(t)

	s := integrationSynth(t, "copy", "copy")
	out := renderSequence(t, s)

	assert.NotEmpty(t, out)
	assertTempEmpty(t, tmp)
}

// A failing external resampler must surface a diagnostic error and leave no
// temporary WAV or output files behind.
func TestIntegrationExternalResamplerFailure(t *testing.T) {
	tmp := pinTempDir(t)

	s := integrationSynth(t, "fail", "copy")
	s.EnqueueSequence(integrationSequence())
	_, err := s.ReadSamples(make([]float32, 4096))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resample")
	assertTempEmpty(t, tmp)
}

// A failing external wavtool must surface a diagnostic error and leave no
// temporary WAV or output files behind.
func TestIntegrationExternalWavtoolFailure(t *testing.T) {
	tmp := pinTempDir(t)

	s := integrationSynth(t, "copy", "fail")
	s.EnqueueSequence(integrationSequence())
	_, err := s.ReadSamples(make([]float32, 4096))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to concatenate")
	assertTempEmpty(t, tmp)
}

// Stopping mid-read must not leave temporary or output files behind, because
// each note's external-command work is fully finished and cleaned up before its
// samples are handed to the caller.
func TestIntegrationInterruptedRead(t *testing.T) {
	tmp := pinTempDir(t)

	s := integrationSynth(t, "copy", "copy")
	s.EnqueueSequence(integrationSequence())

	// Ask for fewer samples than a note produces, then "stop reading".
	buf := make([]float32, 8)
	n, err := s.ReadSamples(buf)
	require.NoError(t, err)
	assert.Equal(t, len(buf), n)

	assertTempEmpty(t, tmp)
}

// A successful external render followed by a retry after failure must produce
// identical audio, and the failed attempt must not leave a cache entry that the
// retry would read.
func TestIntegrationRetryAfterFailure(t *testing.T) {
	pinTempDir(t)

	cacheDir := t.TempDir()
	c := diskcache.New(cacheDir, gotau.ResamplerDiskCacheExt)

	// A single voicebank instance keeps the cache key stable across renders.
	vb := newOnDiskVoicebank(t)
	reference := renderSequence(t, integrationSynthWithCache(t, vb, "copy", "copy", diskcache.New(t.TempDir(), gotau.ResamplerDiskCacheExt)))

	// Failing render: the resampler command fails, so nothing is rendered.
	failing := integrationSynthWithCache(t, vb, "fail", "copy", c)
	failing.EnqueueSequence(integrationSequence())
	_, err := failing.ReadSamples(make([]float32, 4096))
	require.Error(t, err)

	// Retry with the same cache succeeds and matches the reference exactly.
	retry := renderSequence(t, integrationSynthWithCache(t, vb, "copy", "copy", c))
	assert.Equal(t, reference, retry)

	// The retry populated the cache with a usable entry and left no temporaries.
	entries := 0
	require.NoError(t, filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		entries++
		assert.Equal(t, gotau.ResamplerDiskCacheExt, filepath.Ext(d.Name()), "unexpected temporary file: %s", path)
		return nil
	}))
	assert.GreaterOrEqual(t, entries, 1, "the retry should have cached the resampled note")

	// A third render against the same cache must be served from it: no
	// re-render, and identical audio.
	var reported []error
	cached := integrationSynthWithCache(t, vb, "fail", "copy", c) // would fail if it re-rendered
	cached.SetCacheErrorHandler(func(err error) { reported = append(reported, err) })
	assert.Equal(t, reference, renderSequence(t, cached))
	assert.Empty(t, reported, "cache hit should not report errors")
}

// Concurrent renders of the same note through the external resampler and
// wavtool must all produce identical audio, with no temporary files left over.
func TestIntegrationConcurrentRenderSameNote(t *testing.T) {
	tmp := pinTempDir(t)
	setFakeEnv(t, "copy", "copy")

	vb := newOnDiskVoicebank(t)
	cacheDir := t.TempDir()
	shared := diskcache.New(cacheDir, gotau.ResamplerDiskCacheExt)

	reference := renderSequence(t, newIntegrationSynth(t, vb, &cache.NopCache{}))

	const workers = 6
	results := make([][]float32, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newIntegrationSynth(t, vb, shared)
			s.EnqueueSequence(integrationSequence())

			var out []float32
			buf := make([]float32, 64)
			for {
				n, err := s.ReadSamples(buf)
				out = append(out, buf[:n]...)
				if err != nil {
					if !errors.Is(err, io.EOF) {
						errs[i] = err
					}
					results[i] = out
					return
				}
			}
		}()
	}
	wg.Wait()

	for i := range workers {
		require.NoError(t, errs[i])
		assert.Equal(t, reference, results[i], "worker %d produced different audio", i)
	}

	assertTempEmpty(t, tmp)
}
