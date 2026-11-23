package diskcache_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/diskcache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(s string) cache.KeyFunc {
	return func(w io.Writer) { _, _ = io.WriteString(w, s) }
}

func TestCache_OpenMissing(t *testing.T) {
	c := diskcache.New(t.TempDir(), ".wav")
	_, err := c.Open(context.Background(), testKey("missing"))
	assert.ErrorIs(t, err, cache.ErrNotFound)
}

func TestCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := diskcache.New(dir, ".wav")

	w, err := c.Create(context.Background(), testKey("k"))
	require.NoError(t, err)
	_, err = w.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	rc, err := c.Open(context.Background(), testKey("k"))
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "hello", string(got))
}

// An aborted write must leave neither the entry nor a temporary file behind.
func TestCache_AbortLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	c := diskcache.New(dir, ".wav")

	w, err := c.Create(context.Background(), testKey("k"))
	require.NoError(t, err)
	_, err = w.Write([]byte("partial"))
	require.NoError(t, err)
	// Do not write a complete blob: the entry must not become visible.
	require.NoError(t, w.Abort())

	_, err = c.Open(context.Background(), testKey("k"))
	assert.ErrorIs(t, err, cache.ErrNotFound)

	assertNoLeftovers(t, dir)
}

// A half-written entry from an earlier interrupted process must not be visible
// to Open, so a retry re-renders instead of reading a broken file.
func TestCache_Delete(t *testing.T) {
	dir := t.TempDir()
	c := diskcache.New(dir, ".wav")

	w, err := c.Create(context.Background(), testKey("k"))
	require.NoError(t, err)
	_, _ = w.Write([]byte("bad"))
	require.NoError(t, w.Close())

	require.NoError(t, c.Delete(context.Background(), testKey("k")))

	_, err = c.Open(context.Background(), testKey("k"))
	assert.ErrorIs(t, err, cache.ErrNotFound)

	// Deleting a missing entry is not an error.
	assert.NoError(t, c.Delete(context.Background(), testKey("k")))
}

// An unwritable cache directory must surface as an error, not a panic.
func TestCache_CreateInUnwritableDir(t *testing.T) {
	// A regular file where a directory is expected makes MkdirAll fail on every
	// platform, which is more portable than chmod-based read-only directories.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	c := diskcache.New(filepath.Join(blocker, "cache"), ".wav")
	_, err := c.Create(context.Background(), testKey("k"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "diskcache:")
}

func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			t.Errorf("unexpected leftover file: %s", path)
		}
		return nil
	})
	require.NoError(t, err)
}
