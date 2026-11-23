package cache

import (
	"context"
	"io"
)

var _ Cache = (*NopCache)(nil)

// NopCache is a cache that doesn't do anything. It always returns [ErrNotFound] for [Open] and does nothing for [Create].
type NopCache struct{}

func (c *NopCache) Open(_ context.Context, _ KeyFunc) (io.ReadCloser, error) {
	return nil, ErrNotFound
}

func (c *NopCache) Create(_ context.Context, _ KeyFunc) (BlobWriter, error) {
	return &nopBlobWriter{}, nil
}

type nopBlobWriter struct {
	pos int64
}

func (c *nopBlobWriter) Write(p []byte) (int, error) {
	c.pos += int64(len(p))
	return len(p), nil
}

func (c *nopBlobWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.pos = offset
	case io.SeekCurrent:
		c.pos += offset
	case io.SeekEnd:
		c.pos += offset
	}
	return c.pos, nil
}

func (c *nopBlobWriter) Close() error {
	return nil
}

func (c *nopBlobWriter) Abort() error {
	return nil
}
