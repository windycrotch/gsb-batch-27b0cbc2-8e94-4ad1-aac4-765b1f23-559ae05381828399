package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/sfluor/musig/internal/pkg/fingerprint"
	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/sfluor/musig/pkg/dsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AssetsDir points at the wav files used by the test suite (relative to the
// package directory).
const AssetsDir = "../../../assets/test"

func assetPath(name string) string {
	return filepath.Join(AssetsDir, name)
}

func newTestPipeline(t *testing.T) *Pipeline {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pipeline.db")
	p, err := NewDefaultPipeline(dbPath)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// allFingerprints reads every value currently stored for the keys of the given
// fingerprint map.
func allFingerprints(t *testing.T, p *Pipeline, keys []model.EncodedKey) map[model.EncodedKey][]model.TableValue {
	t.Helper()
	stored, err := p.DB.Get(keys)
	require.NoError(t, err)
	return stored
}

func keysOf(fpr map[model.EncodedKey]model.TableValue) []model.EncodedKey {
	keys := make([]model.EncodedKey, 0, len(fpr))
	for k := range fpr {
		keys = append(keys, k)
	}
	return keys
}

func countRecords(stored map[model.EncodedKey][]model.TableValue) int {
	total := 0
	for _, values := range stored {
		total += len(values)
	}
	return total
}

// candidateSongIDs mirrors the query side (see cmd/musig/cmd/read.go): it
// collects the distinct song IDs returned for the given keys. The returned set
// must never contain a song more than once.
func candidateSongIDs(t *testing.T, p *Pipeline, keys []model.EncodedKey) map[uint32]int {
	t.Helper()
	stored, err := p.DB.Get(keys)
	require.NoError(t, err)

	occurrences := map[uint32]int{}
	for _, values := range stored {
		for _, v := range values {
			occurrences[v.SongID]++
		}
	}
	return occurrences
}

// assertFingerprintMatches checks that the database stores exactly one copy of
// every record of the given fingerprint, and nothing extra on those keys.
func assertFingerprintMatches(t *testing.T, p *Pipeline, fpr map[model.EncodedKey]model.TableValue) {
	t.Helper()
	stored := allFingerprints(t, p, keysOf(fpr))
	require.Len(t, stored, len(fpr))
	for k, v := range fpr {
		assert.Equal(t, []model.TableValue{v}, stored[k], "key %v should hold a single record", k)
	}
	assert.Equal(t, len(fpr), countRecords(stored))
}

// TestProcessAndStoreIsIdempotent loads the same file several times and checks
// that the song ID stays stable and the fingerprint table never grows.
func TestProcessAndStoreIsIdempotent(t *testing.T) {
	p := newTestPipeline(t)
	path := assetPath("440.wav")

	first, err := p.ProcessAndStore(path)
	require.NoError(t, err)
	require.NotZero(t, first.SongID)
	require.NotEmpty(t, first.Fingerprint)

	for i := 0; i < 3; i++ {
		res, err := p.ProcessAndStore(path)
		require.NoError(t, err)
		// The song name -> song ID mapping must remain stable.
		assert.Equal(t, first.SongID, res.SongID)
		assert.Equal(t, first.Fingerprint, res.Fingerprint)
	}

	// A single copy of every record of the fingerprint, nothing more.
	assertFingerprintMatches(t, p, first.Fingerprint)

	// The query candidate set contains the song only once.
	occurrences := candidateSongIDs(t, p, keysOf(first.Fingerprint))
	assert.Equal(t, map[uint32]int{first.SongID: len(first.Fingerprint)}, occurrences)
}

// TestSharedKeysAcrossSongs loads two songs known to share fingerprint keys
// and verifies that both songs' records coexist under the shared keys while
// reloading either one does not duplicate anything.
func TestSharedKeysAcrossSongs(t *testing.T) {
	p := newTestPipeline(t)

	songAPath := assetPath("440.wav")
	songBPath := assetPath("440_880.wav")

	resA, err := p.ProcessAndStore(songAPath)
	require.NoError(t, err)
	resB, err := p.ProcessAndStore(songBPath)
	require.NoError(t, err)
	require.NotEqual(t, resA.SongID, resB.SongID)

	// Reload both songs: everything must stay idempotent.
	resAReload, err := p.ProcessAndStore(songAPath)
	require.NoError(t, err)
	resBReload, err := p.ProcessAndStore(songBPath)
	require.NoError(t, err)
	assert.Equal(t, resA.SongID, resAReload.SongID)
	assert.Equal(t, resB.SongID, resBReload.SongID)

	storedA := allFingerprints(t, p, keysOf(resA.Fingerprint))
	storedB := allFingerprints(t, p, keysOf(resB.Fingerprint))

	shared := 0
	for k, va := range resA.Fingerprint {
		vb, ok := resB.Fingerprint[k]
		if !ok {
			// Key only known by song A: it must keep its single record.
			assert.Equal(t, []model.TableValue{va}, storedA[k])
			continue
		}
		shared++
		// Shared key: both songs must be present exactly once.
		assert.ElementsMatch(t, []model.TableValue{va, vb}, storedA[k])
		assert.ElementsMatch(t, []model.TableValue{va, vb}, storedB[k])
	}
	require.NotZero(t, shared, "the two test songs should share at least one fingerprint key")

	// The query side must see both songs, each as a single candidate.
	allKeys := append(keysOf(resA.Fingerprint), keysOf(resB.Fingerprint)...)
	occurrences := candidateSongIDs(t, p, allKeys)
	assert.Contains(t, occurrences, resA.SongID)
	assert.Contains(t, occurrences, resB.SongID)

	// Sanity check the total number of stored records: each song contributes
	// its own fingerprint once, shared keys hold two records.
	totalA := countRecords(storedA)
	totalB := countRecords(storedB)
	assert.Equal(t, len(resA.Fingerprint)+shared, totalA)
	assert.Equal(t, len(resB.Fingerprint)+shared, totalB)
}

// TestMidWriteFailureRetry injects a failure in the middle of the fingerprint
// write and then retries: the final state must match a single successful load.
func TestMidWriteFailureRetry(t *testing.T) {
	p := newTestPipeline(t)
	// Small batches so a 15-58 key fingerprint spans several transactions.
	p.SetFingerprintBatchSize(5)
	path := assetPath("piano.wav")

	p.setFailAfterBatches(2)
	_, err := p.ProcessAndStore(path)
	require.Error(t, err, "the injected failure must surface as an error")

	// The song is registered (a stable ID already exists) but the fingerprint
	// is only partially written.
	songName := model.SongNameFromPath(path)
	songID, err := p.DB.GetSongID(songName)
	require.NoError(t, err)
	require.NotZero(t, songID)

	// Compute what a successful load of the same file would produce.
	expected := computeFingerprint(t, path, songID)

	partial := allFingerprints(t, p, keysOf(expected))
	partialCount := countRecords(partial)
	assert.True(t, partialCount > 0, "some batches should have been committed before the failure")
	assert.True(t, partialCount < len(expected), "the fingerprint must be incomplete after the failure")
	// Nothing partial is corrupted: every stored record belongs to the expected set.
	for k, values := range partial {
		for _, v := range values {
			assert.Equal(t, expected[k], v)
		}
	}

	// Retry without the failure injection: it must complete the missing records.
	p.setFailAfterBatches(0)
	retried, err := p.ProcessAndStore(path)
	require.NoError(t, err)
	assert.Equal(t, songID, retried.SongID, "retrying must reuse the existing song ID")
	assert.Equal(t, expected, retried.Fingerprint)

	// The converged state is exactly one full copy of the fingerprint.
	assertFingerprintMatches(t, p, expected)
}

// TestMultipleFailuresConverge retries across several injected failures and
// checks that every retry converges without duplicates.
func TestMultipleFailuresConverge(t *testing.T) {
	p := newTestPipeline(t)
	p.SetFingerprintBatchSize(3)
	path := assetPath("440_880.wav")

	// Fail progressively later; each retry adds the missing prefix batches.
	for _, failAfter := range []int{1, 2, 3} {
		p.setFailAfterBatches(failAfter)
		if _, err := p.ProcessAndStore(path); err == nil {
			// A nil error is acceptable once the batches are all committed.
		}
	}

	songID, err := p.DB.GetSongID(model.SongNameFromPath(path))
	require.NoError(t, err)
	expected := computeFingerprint(t, path, songID)

	p.setFailAfterBatches(0)
	_, err = p.ProcessAndStore(path)
	require.NoError(t, err)

	assertFingerprintMatches(t, p, expected)
}

// TestIdempotencySurvivesCloseAndReopen closes the database, reopens it on the
// same file and checks that reloading keeps one copy of every record and a
// stable song ID.
func TestIdempotencySurvivesCloseAndReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reopen.db")
	path := assetPath("440.wav")

	p, err := NewDefaultPipeline(dbPath)
	require.NoError(t, err)

	first, err := p.ProcessAndStore(path)
	require.NoError(t, err)
	// Load it a second time before closing.
	second, err := p.ProcessAndStore(path)
	require.NoError(t, err)
	require.Equal(t, first.SongID, second.SongID)
	require.NoError(t, p.DB.Close())

	reopened, err := NewDefaultPipeline(dbPath)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)

	third, err := reopened.ProcessAndStore(path)
	require.NoError(t, err)

	// Same stable ID across close/reopen.
	assert.Equal(t, first.SongID, third.SongID)
	assert.Equal(t, first.Fingerprint, third.Fingerprint)
	assertFingerprintMatches(t, reopened, first.Fingerprint)

	// Candidates still contain the single song only.
	occurrences := candidateSongIDs(t, reopened, keysOf(first.Fingerprint))
	assert.Equal(t, map[uint32]int{first.SongID: len(first.Fingerprint)}, occurrences)
}

// TestFinalStateEqualsSingleSuccessfulLoad proves convergence: a load that
// fails halfway and is retried ends up with the exact same fingerprint set as
// a load that succeeded on the first try on a fresh database.
func TestFinalStateEqualsSingleSuccessfulLoad(t *testing.T) {
	path := assetPath("piano.wav")

	// Reference: one clean successful load on a fresh database.
	ref := newTestPipeline(t)
	refRes, err := ref.ProcessAndStore(path)
	require.NoError(t, err)
	refStored := allFingerprints(t, ref, keysOf(refRes.Fingerprint))

	// Failing then retrying on another fresh database.
	failingPath := filepath.Join(t.TempDir(), "failing.db")
	failing, err := NewDefaultPipeline(failingPath)
	require.NoError(t, err)
	t.Cleanup(failing.Close)
	failing.SetFingerprintBatchSize(4)
	failing.setFailAfterBatches(2)
	_, err = failing.ProcessAndStore(path)
	require.Error(t, err)
	failing.setFailAfterBatches(0)
	retried, err := failing.ProcessAndStore(path)
	require.NoError(t, err)

	require.Equal(t, refRes.SongID, retried.SongID)
	failingStored := allFingerprints(t, failing, keysOf(retried.Fingerprint))

	// Same number of keys and identical record sets under each key.
	require.Len(t, failingStored, len(refStored))
	for k, refValues := range refStored {
		assert.ElementsMatch(t, refValues, failingStored[k])
	}
	assert.Equal(t, countRecords(refStored), countRecords(failingStored))
}

// computeFingerprint runs the same spectrogram + fingerprinting stages as the
// pipeline without touching the database, so tests can compare the stored
// records against the expected fingerprint for a given song ID.
func computeFingerprint(t *testing.T, path string, songID uint32) map[model.EncodedKey]model.TableValue {
	t.Helper()
	p := &Pipeline{
		s:   dsp.NewSpectrogrammer(model.DownsampleRatio, model.MaxFreq, model.SampleSize, true),
		fpr: fingerprint.NewDefaultFingerprinter(),
	}
	partial, err := p.read(path)
	require.NoError(t, err)
	return p.fpr.Fingerprint(songID, partial.cMap)
}
