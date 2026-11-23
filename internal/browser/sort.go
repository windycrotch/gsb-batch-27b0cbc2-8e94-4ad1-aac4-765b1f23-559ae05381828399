package browser

import (
	"cmp"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SortField string

const (
	SortName     SortField = "Name"
	SortModified SortField = "Modified"
	SortCreated  SortField = "Created"
	SortSize     SortField = "Size"
	SortType     SortField = "Type"
)

// Sort always keeps directories first and unavailable metadata last. Name is a
// deterministic tiebreaker regardless of direction; the parent stays first.
func Sort(entries []Entry, field SortField, descending bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Name == ".." || b.Name == ".." {
			return a.Name == ".." && b.Name != ".."
		}
		if a.Dir != b.Dir {
			return a.Dir
		}
		order := 0
		switch field {
		case SortModified, SortCreated:
			at, bt := a.Modified, b.Modified
			if field == SortCreated {
				at, bt = a.Created, b.Created
			}
			if at.IsZero() != bt.IsZero() {
				return !at.IsZero()
			}
			order = at.Compare(bt)
		case SortSize:
			if a.Metadata != b.Metadata {
				return a.Metadata
			}
			order = cmp.Compare(a.Size, b.Size)
		case SortType:
			order = strings.Compare(entryType(a), entryType(b))
		default:
			order = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		if order != 0 {
			if descending {
				return order > 0
			}
			return order < 0
		}
		return a.Name < b.Name
	})
}
func entryType(e Entry) string {
	if e.Symlink {
		return "link"
	}
	if e.Dir {
		return "directory"
	}
	extension := strings.ToLower(filepath.Ext(e.Name))
	if extension == "" {
		return "file"
	}
	return extension
}
func (e Entry) SortValue(field SortField) string {
	switch field {
	case SortModified:
		return dateValue(e.Modified)
	case SortCreated:
		return dateValue(e.Created)
	case SortSize:
		if !e.Metadata {
			return "unavailable"
		}
		// Directory inode sizes are not recursive totals.
		return sizeValue(e.Size)
	case SortType:
		return entryType(e)
	}
	return ""
}
func dateValue(t time.Time) string {
	if t.IsZero() {
		return "unavailable"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func sizeValue(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
