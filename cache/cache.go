package cache

import (
	"context"
	"errors"
	"io"
)

var ErrNotFound = errors.New("cache: not found")

// KeyFunc is a function that writes the key to the provided writer for hashing.
// It is to prevent the need to allocate a []byte slice for the key.
//
// KeyFunc must be deterministic and must write identical bytes each time it is called.
type KeyFunc func(w io.Writer)

// Cache is an interface for a simple key-value store for caching data.
type Cache interface {
	// Open returns a reader for the given key. If the key does not exist, it returns [ErrNotFound].
	Open(ctx context.Context, key KeyFunc) (io.ReadCloser, error)

	// Create returns a writer for the given key. If the key already exists, it should be overwritten.
	Create(ctx context.Context, key KeyFunc) (BlobWriter, error)
}

// BlobWriter is an interface for writing data to the cache. It is returned by [Cache.Create] and should be closed when done.
//
// Data written to a [BlobWriter] must only become visible to [Cache.Open] after a
// successful [BlobWriter.Close]. Implementations must not expose partially
// written entries, and [BlobWriter.Abort] must discard any temporary data.
type BlobWriter interface {
	io.Writer
	io.Seeker
	io.Closer

	// Abort aborts the write operation and discards any temporary data.
	Abort() error
}

// Deleter is an optional interface for caches that are able to remove entries.
//
// The synthesizer uses it on a best-effort basis to discard entries that cannot
// be decoded or that do not match the expected audio format, so that subsequent
// renders do not keep reusing a bad entry. Caches that do not implement it are
// still fully functional; bad entries are simply re-rendered each time.
type Deleter interface {
	// Delete removes the entry for the given key. Removing a missing entry must not be an error.
	Delete(ctx context.Context, key KeyFunc) error
}
