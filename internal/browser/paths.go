package browser

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lancekrogers/peep/internal/media"
)

// ReadPaths refreshes a supplied result set without discovering extra files or
// hiding explicitly supplied dotfiles. Deleted results disappear on refresh.
func ReadPaths(paths []string, base string) ([]Entry, error) {
	entries := make([]Entry, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read result %q: %w", path, err)
		}
		name, err := filepath.Rel(base, path)
		if err != nil || name == ".." {
			name = path
		}
		entry := Entry{Path: path, Name: name, Dir: info.IsDir(), Symlink: info.Mode()&os.ModeSymlink != 0,
			Modified: info.ModTime(), Created: createdTime(path, info), Size: info.Size(), Metadata: true}
		if target, err := os.Stat(path); err == nil {
			entry.Dir = target.IsDir()
			if target.Mode().IsRegular() {
				entry.Kind = media.Classify(path)
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
