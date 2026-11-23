package main

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestReadPaths(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		read0       bool
		want        []string
	}{
		{"newlines and CRLF", "a.md\r\n\n folder/b.txt \nlast", false, []string{"a.md", " folder/b.txt ", "last"}},
		{"NUL preserves newlines and carriage returns", "a\nb\x00 c\r\x00\x00last", true, []string{"a\nb", " c\r", "last"}},
		{"empty", "\n\r\n", false, nil},
		{"empty NUL", "\x00\x00", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPaths(strings.NewReader(tc.input), tc.read0)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
	if _, err := readPaths(strings.NewReader("a\x00b"), false); err == nil || !strings.Contains(err.Error(), "--read0") {
		t.Fatal("missing NUL guidance", err)
	}
	if _, err := readPaths(strings.NewReader(strings.Repeat("a", 1024*1024)), false); err == nil {
		t.Fatal("unbounded path accepted")
	}
	if _, err := readPaths(failingReader{}, false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("lost read error", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
