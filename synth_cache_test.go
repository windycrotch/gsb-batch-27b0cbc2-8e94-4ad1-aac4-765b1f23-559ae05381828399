package gotau_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/SladkyCitron/gotau"
	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/diskcache"
	"github.com/SladkyCitron/gotau/cache/memcache"
	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/gotau/sequence"
	"github.com/SladkyCitron/gotau/umath"
	"github.com/SladkyCitron/gotau/voicebank"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test helpers ---------------------------------------------------------

const testSampleRate = 44100

var binaryLE = binary.LittleEndian

// fakeResampler deterministically renders a note from its configuration, so the
// same note always produces the same samples. It counts calls so tests can tell
// cache hits from re-renders, and can be told to fail to exercise recovery.
type fakeResampler struct {
	id       string
	calls    atomic.Int64
	failNext atomic.Int64
}

func newFakeResampler(id string) *fakeResampler { return &fakeResampler{id: id} }

func (r *fakeResampler) ID() string { return r.id }

func (r *fakeResampler) Resample(_ aio.SampleReader, cfg resampler.ResampleConfig) (aio.SampleReader, error) {
	r.calls.Add(1)
	if r.failNext.Add(-1) >= 0 {
		return nil, errors.New("fake resampler: deliberate failure")
	}
	return &sliceReader{samples: fakeSamples(cfg)}, nil
}

// fakeSamples produces deterministic output that depends on the resample config,
// so rendering with different parameters is observably different.
func fakeSamples(cfg resampler.ResampleConfig) []float32 {
	n := int(cfg.Length / 1000 * testSampleRate)
	n = max(0, min(n, 20000))
	out := make([]float32, n)
	amp := (float64(cfg.Pitch%12) + 1) / 12
	for i := range out {
		out[i] = float32(math.Sin(float64(i)*0.01) * amp)
	}
	return out
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

func testFormat(sr int) afmt.Format {
	return afmt.Format{SampleRate: freq.Frequency(sr) * freq.Hertz, NumChannels: 1}
}

// newTestVoicebank builds a minimal in-memory voicebank with one oto entry
// pointing at a valid mono WAV sample.
func newTestVoicebank(t *testing.T) *voicebank.Voicebank {
	t.Helper()

	mfs := fstest.MapFS{
		"oto.ini":    &fstest.MapFile{Data: []byte("sample.wav=a,0,120,-39,80,20\n")},
		"sample.wav": &fstest.MapFile{Data: encodeTestWav(t, make([]float32, testSampleRate/10))},
	}

	vb, err := voicebank.Open(mfs)
	require.NoError(t, err)
	return vb
}

func encodeTestWav(t *testing.T, samples []float32) []byte {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "test-*.wav")
	require.NoError(t, err)

	enc, err := wav.NewEncoder(f, testFormat(testSampleRate), afmt.SampleFormat{
		BitDepth: 16,
		Encoding: afmt.SampleEncodingInt,
		Endian:   binaryLE,
	}, wav.FormatInt)
	require.NoError(t, err)
	_, err = enc.WriteSamples(samples)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	require.NoError(t, f.Close())

	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return data
}

func testSequence() sequence.Sequence {
	return sequence.Sequence{
		Metadata: sequence.Metadata{Resolution: 480, Tempo: 120},
		Notes: []sequence.Note{{
			Position: 0,
			Duration: 480,
			Lyric:    "a",
			Note:     60,
			Velocity: 100,
			Envelope: testEnvelope(),
		}},
	}
}

// testEnvelope is a plain 4-point envelope so that sequences render correctly
// with external concatenators too.
func testEnvelope() umath.Envelope {
	return umath.Envelope{
		{X: 0, Y: 0},
		{X: 0, Y: 1},
		{X: 100, Y: 1},
		{X: 200, Y: 0},
	}
}

// renderSequence renders the standard test sequence and returns all samples.
func renderSequence(t *testing.T, s *gotau.Synth) []float32 {
	t.Helper()

	s.EnqueueSequence(testSequence())
	var out []float32
	buf := make([]float32, 64)
	for {
		n, err := s.ReadSamples(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("render failed: %v", err)
		}
	}
}

func newTestSynth(t *testing.T, res resampler.Resampler, c cache.Cache) *gotau.Synth {
	t.Helper()
	s := gotau.New(testSampleRate, newTestVoicebank(t), res, &concat.Append{})
	s.SetResamplerCache(c)
	return s
}

// --- tests ----------------------------------------------------------------

// A cache directory that cannot be written must not lose the rendered audio.
func TestSynthCacheWriteFailureKeepsAudio(t *testing.T) {
	reference := renderSequence(t, newTestSynth(t, newFakeResampler("ref"), &cache.NopCache{}))

	res := newFakeResampler("failwrite")
	var reported []error
	s := newTestSynth(t, res, &failingCache{
		openErr:   cache.ErrNotFound,
		createErr: errors.New("cache directory is read-only"),
	})
	s.SetCacheErrorHandler(func(err error) { reported = append(reported, err) })

	got := renderSequence(t, s)

	assert.Equal(t, reference, got, "rendered audio must be unaffected by cache failures")
	assert.Equal(t, int64(1), res.calls.Load())
	require.NotEmpty(t, reported, "cache failure should be reported for diagnostics")
	assert.Contains(t, reported[0].Error(), "cache")
}

// A cache that errors on reads must also fall back to rendering.
func TestSynthCacheReadFailureKeepsAudio(t *testing.T) {
	reference := renderSequence(t, newTestSynth(t, newFakeResampler("ref"), &cache.NopCache{}))

	s := newTestSynth(t, newFakeResampler("failread"), &failingCache{
		openErr:   errors.New("permission denied"),
		createErr: errors.New("permission denied"),
	})

	assert.Equal(t, reference, renderSequence(t, s))
}

// A cache writer that fails mid-write must leave no entry behind, and a later
// successful render must heal the cache.
func TestSynthCacheAbortedWriteLeavesNoEntry(t *testing.T) {
	reference := renderSequence(t, newTestSynth(t, newFakeResampler("ref"), &cache.NopCache{}))

	c := newMapCache()
	c.writeErr = errors.New("disk full")
	s := newTestSynth(t, newFakeResampler("abort"), c)

	assert.Equal(t, reference, renderSequence(t, s))
	assert.Empty(t, c.keys(), "an aborted cache write must not store an entry")

	c.writeErr = nil
	assert.Equal(t, reference, renderSequence(t, newTestSynth(t, newFakeResampler("abort"), c)))
	require.Len(t, c.keys(), 1)
	assert.NotEmpty(t, c.get(c.keys()[0]))
}

// A corrupt entry must never be reused: it is invalidated, the note is
// re-rendered, and a good entry is written back.
func TestSynthCorruptCacheEntryIsInvalidated(t *testing.T) {
	res := newFakeResampler("corrupt")
	c := newMapCache()

	first := renderSequence(t, newTestSynth(t, res, c))
	require.Len(t, c.keys(), 1)
	key := c.keys()[0]

	// Simulate a half-written / truncated entry from an interrupted write.
	c.set(key, []byte("RIFF-not-really-a-wav"))

	var reported []error
	s := newTestSynth(t, res, c)
	s.SetCacheErrorHandler(func(err error) { reported = append(reported, err) })

	second := renderSequence(t, s)
	assert.Equal(t, first, second, "a corrupt entry must fall back to fresh audio")
	assert.Equal(t, int64(2), res.calls.Load(), "corrupt entry should force a re-render")
	require.NotEmpty(t, reported)
	assert.Contains(t, reported[0].Error(), "cached audio")

	// The good entry written back must be consumed on the next render.
	third := renderSequence(t, newTestSynth(t, res, c))
	assert.Equal(t, first, third)
	assert.Equal(t, int64(2), res.calls.Load(), "healed entry should be a cache hit")
}

// A successful render followed by a cache hit must produce identical samples.
func TestSynthCacheHitIsIdentical(t *testing.T) {
	res := newFakeResampler("hit")
	c := memcache.New()

	first := renderSequence(t, newTestSynth(t, res, c))
	second := renderSequence(t, newTestSynth(t, res, c))

	assert.Equal(t, first, second)
	assert.Equal(t, int64(1), res.calls.Load(), "second render should be served from cache")
}

// A render that fails in the resampler must not poison the cache; retrying the
// same sequence must yield exactly the audio of a first-time success.
func TestSynthRetryAfterFailureMatchesSuccessfulRender(t *testing.T) {
	reference := renderSequence(t, newTestSynth(t, newFakeResampler("ref"), &cache.NopCache{}))

	c := memcache.New()

	// First attempt: the resampler fails, so the render errors out.
	failing := newFakeResampler("retry")
	failing.failNext.Store(1)
	s1 := newTestSynth(t, failing, c)
	s1.EnqueueSequence(testSequence())
	_, err := s1.ReadSamples(make([]float32, 4096))
	require.Error(t, err, "a resampler failure should surface as a render error")

	// Second attempt: render again from scratch against the same cache.
	retry := renderSequence(t, newTestSynth(t, newFakeResampler("retry"), c))
	assert.Equal(t, reference, retry, "retry must not reuse partial cache data")
}

// Concurrent renders of the same note must all succeed with identical audio and
// must not leave temporary files behind.
func TestSynthConcurrentRenderSameNote(t *testing.T) {
	reference := renderSequence(t, newTestSynth(t, newFakeResampler("ref"), &cache.NopCache{}))

	cacheDir := t.TempDir()
	shared := diskcache.New(cacheDir, gotau.ResamplerDiskCacheExt)

	const workers = 8
	var wg sync.WaitGroup
	results := make([][]float32, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := gotau.New(testSampleRate, newTestVoicebank(t), newFakeResampler("concurrent"), &concat.Append{})
			s.SetResamplerCache(shared)
			s.EnqueueSequence(testSequence())

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

	assertNoTempFiles(t, cacheDir)
}

func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() && filepath.Ext(d.Name()) != gotau.ResamplerDiskCacheExt {
			t.Errorf("unexpected leftover file in cache tree: %s", path)
		}
		return nil
	})
	require.NoError(t, err)
}

// --- cache doubles --------------------------------------------------------
// failingCache fails every operation, simulating an unwritable cache directory.
type failingCache struct {
	openErr   error
	createErr error
}

func (c *failingCache) Open(context.Context, cache.KeyFunc) (io.ReadCloser, error) {
	return nil, c.openErr
}

func (c *failingCache) Create(context.Context, cache.KeyFunc) (cache.BlobWriter, error) {
	return nil, c.createErr
}

// mapCache is an in-memory cache keyed by the raw key bytes, which lets tests
// inspect and corrupt entries directly. It implements [cache.Deleter].
type mapCache struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	writeErr error
}

func newMapCache() *mapCache { return &mapCache{blobs: make(map[string][]byte)} }

func keyBytes(key cache.KeyFunc) string {
	var buf bytes.Buffer
	key(&buf)
	return buf.String()
}

func (c *mapCache) Open(_ context.Context, key cache.KeyFunc) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.blobs[keyBytes(key)]
	if !ok {
		return nil, cache.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (c *mapCache) Create(_ context.Context, key cache.KeyFunc) (cache.BlobWriter, error) {
	return &mapBlobWriter{c: c, key: keyBytes(key)}, nil
}

func (c *mapCache) Delete(_ context.Context, key cache.KeyFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.blobs, keyBytes(key))
	return nil
}

func (c *mapCache) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.blobs))
	for k := range c.blobs {
		out = append(out, k)
	}
	return out
}

func (c *mapCache) get(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blobs[key]
}

func (c *mapCache) set(key string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[key] = b
}

// mapBlobWriter buffers a whole entry in memory and commits it on Close. It
// supports random access because the WAV encoder seeks back to patch header
// sizes.
type mapBlobWriter struct {
	c    *mapCache
	key  string
	buf  testBuffer
	done bool
}

func (w *mapBlobWriter) Write(p []byte) (int, error) {
	if w.c.writeErr != nil {
		return 0, w.c.writeErr
	}
	return w.buf.Write(p)
}

func (w *mapBlobWriter) Seek(offset int64, whence int) (int64, error) {
	return w.buf.Seek(offset, whence)
}

func (w *mapBlobWriter) Close() error {
	if w.done {
		return nil
	}
	w.done = true
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	w.c.blobs[w.key] = append([]byte(nil), w.buf.Bytes()...)
	return nil
}

func (w *mapBlobWriter) Abort() error {
	w.done = true
	w.buf.Reset()
	return nil
}

// testBuffer is a minimal growable, seekable byte buffer.
type testBuffer struct {
	b   []byte
	pos int
}

func (b *testBuffer) Write(p []byte) (int, error) {
	if need := b.pos + len(p); need > len(b.b) {
		grown := make([]byte, need)
		copy(grown, b.b)
		b.b = grown
	}
	copy(b.b[b.pos:], p)
	b.pos += len(p)
	return len(p), nil
}

func (b *testBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.pos = int(offset)
	case io.SeekCurrent:
		b.pos += int(offset)
	case io.SeekEnd:
		b.pos = len(b.b) + int(offset)
	default:
		return 0, fmt.Errorf("unsupported whence: %d", whence)
	}
	if b.pos < 0 {
		return 0, errors.New("negative position")
	}
	return int64(b.pos), nil
}

func (b *testBuffer) Bytes() []byte { return b.b }

func (b *testBuffer) Reset() { b.b = nil; b.pos = 0 }
