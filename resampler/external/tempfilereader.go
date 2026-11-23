package external

import (
	"errors"
	"os"
)

// tempFileReader reads a temporary file and removes it when closed.
type tempFileReader struct {
	f *os.File
}

func openTemp(name string) (*tempFileReader, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &tempFileReader{f: f}, nil
}

func (r *tempFileReader) Read(p []byte) (int, error) {
	return r.f.Read(p)
}

// Close closes and removes the temporary file. Removing a file that has already
// been removed (e.g. because its whole temporary directory was cleaned up) is
// not treated as an error.
func (r *tempFileReader) Close() error {
	closeErr := r.f.Close()
	if err := os.Remove(r.f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		if closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}
