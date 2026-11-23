package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

func readPaths(input io.Reader, read0 bool) ([]string, error) {
	delimiter := byte('\n')
	if read0 {
		delimiter = 0
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, delimiter); i >= 0 {
			return i + 1, data[:i], nil
		}
		if eof && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var paths []string
	for scanner.Scan() {
		path := scanner.Text()
		if !read0 {
			path = strings.TrimSuffix(path, "\r") // Accept CRLF lists.
			if strings.ContainsRune(path, 0) {
				return nil, fmt.Errorf("NUL in stdin file list; use --read0 with rg -l -0 or find -print0")
			}
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stdin file list: %w", err)
	}
	return paths, nil
}
