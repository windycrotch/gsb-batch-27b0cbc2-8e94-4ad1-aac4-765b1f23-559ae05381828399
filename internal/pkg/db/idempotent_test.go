package db

import (
	"path/filepath"
	"testing"

	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// newTestBoltDB opens a fresh bolt database in a temporary directory and
// registers a cleanup closing (and removing) it.
func newTestBoltDB(t *testing.T) *BoltDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := NewBoltDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

func fingerprintKeys(m map[model.EncodedKey]model.TableValue) []model.EncodedKey {
	keys := make([]model.EncodedKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// countStoredTableValues walks the fingerprint bucket and returns the total
// number of serialized TableValue records stored under all keys.
func countStoredTableValues(t *testing.T, database *BoltDB) int {
	t.Helper()
	total := 0
	err := database.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(database.fingerprintBucket)
		return b.ForEach(func(_, raw []byte) error {
			total += len(raw) / model.TableValueSize
			return nil
		})
	})
	require.NoError(t, err)
	return total
}

// countStoredSongs returns the number of records in the song bucket.
func countStoredSongs(t *testing.T, database *BoltDB) int {
	t.Helper()
	total := 0
	err := database.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(database.songBucket)
		return b.ForEach(func(_, _ []byte) error {
			total++
			return nil
		})
	})
	require.NoError(t, err)
	return total
}

// TestSetIsIdempotent ensures that writing the same (key, value) pairs several
// times never duplicates the stored records.
func TestSetIsIdempotent(t *testing.T) {
	database := newTestBoltDB(t)

	batch := genTestMap(testValues)

	// Load the same batch three times, the stored state must not grow.
	for i := 0; i < 3; i++ {
		require.NoError(t, database.Set(batch))
	}

	keys := fingerprintKeys(batch)
	stored, err := database.Get(keys)
	require.NoError(t, err)
	require.Len(t, stored, len(batch))

	for k, v := range batch {
		// A single record per key: no duplicate entry for the same song.
		assert.Equal(t, []model.TableValue{v}, stored[k])
	}
	assert.Equal(t, len(batch), countStoredTableValues(t, database))
}

// TestSetKeepsOtherSongsOnSharedKey ensures that records from different songs
// sharing the same fingerprint key coexist: de-duplication must only suppress
// exact duplicates, never other songs' data.
func TestSetKeepsOtherSongsOnSharedKey(t *testing.T) {
	database := newTestBoltDB(t)

	const sharedKey model.EncodedKey = 42
	songA := model.TableValue{AnchorTimeMs: 100, SongID: 1}
	songB := model.TableValue{AnchorTimeMs: 100, SongID: 2}
	songC := model.TableValue{AnchorTimeMs: 250, SongID: 1}

	// Two songs sharing the same key with identical anchor times.
	require.NoError(t, database.Set(map[model.EncodedKey]model.TableValue{
		sharedKey: songA,
	}))
	require.NoError(t, database.Set(map[model.EncodedKey]model.TableValue{
		sharedKey: songB,
	}))
	// Re-writing song A must be a no-op while song C (same song, different
	// anchor time) still gets added.
	require.NoError(t, database.Set(map[model.EncodedKey]model.TableValue{
		sharedKey: songA,
	}))
	require.NoError(t, database.Set(map[model.EncodedKey]model.TableValue{
		sharedKey: songC,
	}))

	stored, err := database.Get([]model.EncodedKey{sharedKey})
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]model.TableValue{songA, songB, songC},
		stored[sharedKey],
	)
	assert.Equal(t, 3, countStoredTableValues(t, database))
}

// TestSetSongIsStableAcrossRepeatedCalls ensures the song name -> song ID
// registration is repeatable without allocating new IDs.
func TestSetSongIsStableAcrossRepeatedCalls(t *testing.T) {
	database := newTestBoltDB(t)

	const name = "repeated song"

	firstID, err := database.SetSong(name)
	require.NoError(t, err)
	require.NotZero(t, firstID)

	for i := 0; i < 4; i++ {
		id, err := database.SetSong(name)
		require.NoError(t, err)
		assert.Equal(t, firstID, id)
	}

	// Only one song record must exist.
	assert.Equal(t, 1, countStoredSongs(t, database))

	gotName, err := database.GetSong(firstID)
	require.NoError(t, err)
	assert.Equal(t, name, gotName)

	gotID, err := database.GetSongID(name)
	require.NoError(t, err)
	assert.Equal(t, firstID, gotID)
}

// TestPersistenceAfterReopen ensures the de-duplication result and the stable
// song ID survive closing and reopening the database file.
func TestPersistenceAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")

	database, err := NewBoltDB(path)
	require.NoError(t, err)

	batch := genTestMap(testValues)
	require.NoError(t, database.Set(batch))
	require.NoError(t, database.Set(batch))

	const name = "persisted song"
	id, err := database.SetSong(name)
	require.NoError(t, err)
	_, err = database.SetSong(name)
	require.NoError(t, err)

	require.NoError(t, database.Close())

	// Reopen the same file.
	reopened, err := NewBoltDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	// The duplicates written before closing must not be there.
	keys := fingerprintKeys(batch)
	stored, err := reopened.Get(keys)
	require.NoError(t, err)
	require.Len(t, stored, len(batch))
	for k, v := range batch {
		assert.Equal(t, []model.TableValue{v}, stored[k])
	}
	assert.Equal(t, len(batch), countStoredTableValues(t, reopened))

	// The song keeps its original ID instead of getting a new one.
	reloadedID, err := reopened.SetSong(name)
	require.NoError(t, err)
	assert.Equal(t, id, reloadedID)
	assert.Equal(t, 1, countStoredSongs(t, reopened))
}

// TestRepeatedSetAfterReopenKeepsSingleCopy combines de-duplication with
// reopen: writing again on a reopened database must not duplicate records.
func TestRepeatedSetAfterReopenKeepsSingleCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen_rewrite.db")

	database, err := NewBoltDB(path)
	require.NoError(t, err)

	batch := genTestMap(testValues)
	require.NoError(t, database.Set(batch))
	require.NoError(t, database.Close())

	reopened, err := NewBoltDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	require.NoError(t, reopened.Set(batch))
	require.NoError(t, reopened.Set(batch))

	assert.Equal(t, len(batch), countStoredTableValues(t, reopened))
}
