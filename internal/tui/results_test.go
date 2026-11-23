package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lancekrogers/peep/internal/browser"
)

func TestResultsBrowseAndRefresh(t *testing.T) {
	base := t.TempDir()
	paths := []string{filepath.Join(base, "z", "same.txt"), filepath.Join(base, "a", "same.txt"), filepath.Join(base, ".hidden")}
	for _, path := range append(append([]string(nil), paths...), filepath.Join(base, "unrelated")) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("preview"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewResults(context.Background(), nil, append(paths, paths[0]), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.clearDocument()
	m.resultBase, m.dir = base, base
	refresh := func() { m.Update(m.readDir()()) }
	refresh()
	got := func() []string {
		var names []string
		for _, e := range m.entries {
			names = append(names, e.Name)
		}
		return names
	}
	if want := []string{"z/same.txt", "a/same.txt", ".hidden"}; !reflect.DeepEqual(got(), want) {
		t.Fatalf("input order, deduplication or hidden files lost: %v", got())
	}
	if m.sortLabel() != "Input order" || m.parent() != nil {
		t.Fatal("results escaped into parent directory")
	}
	m.filter = "*.txt"
	m.refilter()
	if len(m.entries) != 2 {
		t.Fatal("basename glob did not match results", got())
	}
	m.sortKey("n")
	if got()[0] != "a/same.txt" {
		t.Fatal("explicit sort ignored", got())
	}
	m.filter = ""
	m.sortKey("i")
	if m.sortExplicit || got()[0] != "z/same.txt" {
		t.Fatal("input order was not restored", got())
	}
	m.cursor = 0
	m.beginFileOperation("rename")
	if m.filePrompt.input != "same.txt" {
		t.Fatal("rename input includes parent folders", m.filePrompt.input)
	}
	m.filePrompt = nil
	newPath := filepath.Join(base, "z", "renamed.txt")
	if err := os.Rename(paths[0], newPath); err != nil {
		t.Fatal(err)
	}
	m.Update(m.applyFileOperation(fileOperationMsg{from: paths[0], to: newPath, operation: "rename"})())
	if m.entries[0].Path != newPath {
		t.Fatal("renamed result lost")
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	m.Update(m.applyFileOperation(fileOperationMsg{from: newPath, operation: "delete"})())
	if len(m.entries) != 2 || len(m.resultPaths) != 2 {
		t.Fatal("deleted result retained")
	}
	if err := os.Remove(paths[1]); err != nil {
		t.Fatal(err)
	}
	refresh()
	if len(m.entries) != 1 || m.entries[0].Name != ".hidden" {
		t.Fatal("refresh did not remove missing file or introduced unrelated paths", got())
	}
}

func TestResultsDirectoriesAndValidation(t *testing.T) {
	base := t.TempDir()
	m, err := NewResults(context.Background(), nil, []string{base}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	m.Update(m.readDir()())
	m.Update(m.navigate(base)())
	if m.resultsMode || m.dir != base || m.parentHint() != "‹ Backspace results" {
		t.Fatal("directory did not open from results")
	}
	m.Update(m.parent()())
	if !m.resultsMode || len(m.entries) != 1 || m.entries[0].Path != base {
		t.Fatal("did not return to result set")
	}
	if _, err := NewResults(context.Background(), nil, []string{filepath.Join(base, "missing")}, false, ""); err == nil {
		t.Fatal("missing input accepted")
	}
	if _, err := NewResults(context.Background(), nil, []string{base}, true, ""); err == nil {
		t.Fatal("directory accepted for piped playback")
	}
	paths := []string{filepath.Join(base, "b.mp3"), filepath.Join(base, "a.mp3")}
	for _, path := range paths {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, err = NewResults(context.Background(), nil, paths, true, "")
	if err != nil || !reflect.DeepEqual(m.queue, paths) || m.index != -2 {
		t.Fatal("piped playback order", m, err)
	}
	entries, err := browser.ReadPaths([]string{filepath.Dir(base)}, base)
	if err != nil || entries[0].Name == ".." {
		t.Fatal("supplied parent directory confused with navigation entry")
	}
}
