package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.mp3", "a.PNG", ".hidden.jpg", "ignore.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || !entries[0].Dir || entries[1].Name != "a.PNG" {
		t.Fatalf("unexpected listing: %+v", entries)
	}
	entries, err = Read(dir, true, "HIDDEN")
	if err != nil || len(entries) != 1 {
		t.Fatalf("filter/hidden: %+v %v", entries, err)
	}
	if _, err = Read(filepath.Join(dir, "missing"), false, ""); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestFilterFoldersAndFiles(t *testing.T) {
	entries := []Entry{{Name: "20261114-222940-work-however-you-like", Dir: true}, {Name: "work.flac"}, {Name: "walk-home.mp4"}, {Name: "unrelated.png"}}
	got := Filter(entries, "WORK")
	if len(got) != 2 || got[0].Name != "work.flac" {
		t.Fatalf("substring priority: %+v", got)
	}
	got = Filter(entries, "wrk hwevr")
	if len(got) != 1 || !got[0].Dir {
		t.Fatalf("fuzzy directory search: %+v", got)
	}
	if got := Filter(entries, "zzzz"); len(got) != 0 {
		t.Fatal("unmatched filter returned entries")
	}
	if len(Filter(entries, "")) != len(entries) {
		t.Fatal("clearing filter lost entries")
	}
}

func TestSymlinkIdentity(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "target.txt"), []byte("text"), 0600)
	os.Mkdir(filepath.Join(dir, "target-dir"), 0700)
	for link, target := range map[string]string{"file-link": "target.txt", "dir-link": "target-dir", "dangling": "absent"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Read(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		expected := entry.Name == "file-link" || entry.Name == "dir-link" || entry.Name == "dangling"
		if entry.Symlink != expected {
			t.Fatalf("symlink flag: %+v", entry)
		}
		if entry.Name == "dir-link" && !entry.Dir {
			t.Fatal("directory link no longer navigable")
		}
	}
}

func TestGlobFiltering(t *testing.T) {
	entries := []Entry{{Name: "rec-folder", Dir: true}, {Name: "rec-one.txt"}, {Name: "REC-two.TXT"}, {Name: "photo-01.jpg"}, {Name: "two words.txt"}, {Name: "other.go"}}
	for _, tc := range []struct {
		query string
		count int
	}{{"rec-*", 3}, {"*.txt", 3}, {"photo-??.jpg", 1}, {"rec-[ot]*", 2}, {"two *.txt", 1}, {"*", 6}, {"[", 0}, {"none*", 0}, {"two wrds", 1}} {
		if got := Filter(entries, tc.query); len(got) != tc.count {
			t.Fatalf("%s: %+v", tc.query, got)
		}
	}
	if FilterError("[") == nil || FilterError("*.txt") != nil {
		t.Fatal("invalid glob diagnostic")
	}
}
