package browser

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSortMetadataAndMissingValues(t *testing.T) {
	old, newer := time.Unix(100, 0), time.Unix(200, 0)
	original := []Entry{{Name: "unknown.txt"}, {Name: "large.txt", Metadata: true, Size: 300, Modified: old, Created: newer}, {Name: "small.go", Metadata: true, Size: 10, Modified: newer, Created: old}, {Name: "directory", Dir: true}, {Name: "..", Dir: true}}
	for _, tc := range []struct {
		field SortField
		desc  bool
		first string
	}{{SortModified, true, "small.go"}, {SortModified, false, "large.txt"}, {SortCreated, true, "large.txt"}, {SortCreated, false, "small.go"}, {SortSize, true, "large.txt"}, {SortSize, false, "small.go"}, {SortType, false, "small.go"}} {
		entries := append([]Entry(nil), original...)
		Sort(entries, tc.field, tc.desc)
		if entries[0].Name != ".." || entries[1].Name != "directory" || entries[2].Name != tc.first {
			t.Fatal(tc, entries)
		}
		if tc.field != SortType && entries[len(entries)-1].Name != "unknown.txt" {
			t.Fatal("missing metadata must sort last")
		}
	}
	if original[0].SortValue(SortCreated) != "unavailable" {
		t.Fatal("fabricated birth date")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "file.txt")
	os.WriteFile(p, []byte("12345"), 0600)
	os.Chtimes(p, old, old)
	entries, err := Read(dir, false, "")
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if entries[0].Size != 5 || !entries[0].Metadata || !entries[0].Modified.Equal(old) {
		t.Fatal(entries)
	}
}
