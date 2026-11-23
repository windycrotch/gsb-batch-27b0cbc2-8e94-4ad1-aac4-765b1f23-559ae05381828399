package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lancekrogers/peep/internal/media"
)

type Entry struct {
	Path, Name        string
	Dir               bool
	Symlink           bool
	Kind              media.Kind
	Modified, Created time.Time
	Size              int64
	Metadata          bool
}

func Read(path string, hidden bool, filter string) ([]Entry, error) {
	dirs, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	entries := []Entry{}
	for _, d := range dirs {
		if !hidden && strings.HasPrefix(d.Name(), ".") {
			continue
		}
		full := filepath.Join(path, d.Name())
		dir := d.IsDir()
		if d.Type()&os.ModeSymlink != 0 {
			if stat, e := os.Stat(full); e == nil {
				dir = stat.IsDir()
			}
		}
		kind := media.Classify(full)
		if st, err := os.Stat(full); err != nil || !st.Mode().IsRegular() {
			kind = ""
		}
		entry := Entry{Path: full, Name: d.Name(), Dir: dir, Symlink: d.Type()&os.ModeSymlink != 0, Kind: kind}
		if info, err := d.Info(); err == nil {
			entry.Metadata = true
			entry.Modified, entry.Size, entry.Created = info.ModTime(), info.Size(), createdTime(full, info)
		}
		entries = append(entries, entry)
	}
	Sort(entries, SortName, false)
	if err := FilterError(filter); err != nil {
		return nil, fmt.Errorf("invalid filename pattern: %w", err)
	}
	return Filter(entries, filter), nil
}
