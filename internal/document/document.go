// Package document reads local files and lays out text without involving playback.
package document

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 2 << 20

type Document struct {
	Path, Source, Kind string
	Truncated          bool
}

func Kind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown":
		return "MD"
	case ".mmd", ".mermaid":
		return "MMD"
	case ".txt", ".log", ".go", ".rs", ".zig", ".py", ".js", ".ts", ".tsx", ".jsx", ".json", ".yaml", ".yml", ".toml", ".sh", ".c", ".h", ".cpp", ".css", ".html", ".xml", ".csv", ".ini", ".conf":
		return "TEXT"
	}
	return "FILE"
}

// OpenRegular uses nonblocking open before checking the descriptor, so a FIFO
// or a symlink swapped between listing and reading cannot hang the preview.
func OpenRegular(path string) (*os.File, error) {
	f, err := openNonblocking(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s · %d bytes · no preview for special files", st.Mode().String(), st.Size())
	}
	return f, nil
}

func Read(ctx context.Context, path string) (*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	chunk := make([]byte, 32<<10)
	for b.Len() <= MaxBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, e := f.Read(chunk[:min(len(chunk), MaxBytes+1-b.Len())])
		b.Write(chunk[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("read file: %w", e)
		}
	}
	data := b.Bytes()
	truncated := len(data) > MaxBytes
	if truncated {
		data = data[:MaxBytes]
		// At most three trailing bytes can belong to an incomplete UTF-8 rune.
		for trim := 0; trim < 3 && len(data) > 0 && !utf8.Valid(data); trim++ {
			data = data[:len(data)-1]
		}
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		st, _ := f.Stat()
		size := int64(len(data))
		if st != nil {
			size = st.Size()
		}
		return nil, fmt.Errorf("binary or non-UTF-8 file · %d bytes · text preview unavailable", size)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	source = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			return '�'
		}
		return r
	}, source)
	return &Document{Path: path, Source: source, Kind: Kind(path), Truncated: truncated}, nil
}
