package mp4

import (
	"bytes"
	"io"
	"os"

	"github.com/alfg/mp4/atom"
)

// Open opens a file and returns an &Mp4Reader{}.
//
// On a partial parse the reader is still returned together with a
// *atom.PartialError; only a total failure (atom.ErrNoValidBoxes) yields a
// nil reader.
func Open(path string) (f *atom.Mp4Reader, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	f = &atom.Mp4Reader{
		Reader: file,
	}
	return finalize(f, f.Parse())
}

// OpenFromReader read and returns an &Mp4Reader{}.
func OpenFromReader(reader io.ReaderAt, size int64) (f *atom.Mp4Reader, err error) {
	f = &atom.Mp4Reader{
		Reader: reader,
		Size:   int64(size),
	}
	return finalize(f, f.Parse())
}

// OpenFromBytes read and returns an &Mp4Reader{}.
func OpenFromBytes(buffer []byte) (f *atom.Mp4Reader, err error) {
	f = &atom.Mp4Reader{
		Reader: bytes.NewReader(buffer),
		Size:   int64(len(buffer)),
	}
	return finalize(f, f.Parse())
}

// finalize keeps the reader on partial failure and drops it on total failure.
func finalize(f *atom.Mp4Reader, err error) (*atom.Mp4Reader, error) {
	if err != nil && !atom.IsPartial(err) {
		return nil, err
	}
	return f, err
}
