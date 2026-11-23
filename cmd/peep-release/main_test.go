package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, missing string) string {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll("packaging/npm", 0755))
	must(writeJSON("packaging/metadata.json", metadata{"test/peep", "@test/peep", "peep-tui-bin", "MIT"}))
	for _, p := range []string{"LICENSE", "packaging/npm/peep.cjs", "packaging/npm/README.md"} {
		must(os.WriteFile(p, []byte("fixture\n"), 0644))
	}
	must(os.MkdirAll("archives", 0755))
	for _, target := range targets {
		path := filepath.Join("archives", fmt.Sprintf("peep_1.2.3_%s_%s.tar.gz", target.OS, target.Arch))
		f, err := os.Create(path)
		must(err)
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		for _, name := range []string{"bin/peep", "LICENSE", "licenses/MMDG-LICENSE.txt", "licenses/MMDG-NOTICES.txt", "../../escape"} {
			if name == missing {
				continue
			}
			must(tw.WriteHeader(&tar.Header{Name: name, Size: 8, Mode: 0755, Typeflag: tar.TypeReg}))
			_, err = tw.Write([]byte("fixture\n"))
			must(err)
		}
		must(tw.Close())
		must(gz.Close())
		must(f.Close())
		b, err := os.ReadFile(path)
		must(err)
		must(os.WriteFile(path+".sha256", []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(b), filepath.Base(path))), 0644))
	}
	return root
}
func TestGenerate(t *testing.T) {
	root := fixture(t, "")
	if err := generate("1.2.3", "archives", "out"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("out/npm/peep/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ OptionalDependencies map[string]string }
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.OptionalDependencies) != 4 {
		t.Fatalf("dependencies: %v", m.OptionalDependencies)
	}
	for _, target := range targets {
		name := "@test/peep-" + target.OS + "-" + target.NodeArch
		if m.OptionalDependencies[name] != "1.2.3" {
			t.Fatalf("missing exact version for %s", name)
		}
		st, err := os.Stat("out/npm/" + target.OS + "-" + target.NodeArch + "/bin/peep")
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0755 {
			t.Fatal("binary not executable")
		}
	}
	if _, err = os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatal("unexpected extracted path")
	}
	if err = generate("1.2.3", "archives", "out"); err == nil {
		t.Fatal("existing output overwritten")
	}
}
func TestRejectsIncompleteRelease(t *testing.T) {
	for _, missing := range []string{"LICENSE", "bin/peep", "licenses/MMDG-NOTICES.txt"} {
		t.Run(missing, func(t *testing.T) {
			fixture(t, missing)
			if err := generate("1.2.3", "archives", "out"); err == nil || !strings.Contains(err.Error(), "archive missing") {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat("out"); !os.IsNotExist(err) {
				t.Fatal("partial output visible")
			}
		})
	}
}
func TestRejectsTamperedArchive(t *testing.T) {
	fixture(t, "")
	f, err := os.OpenFile("archives/peep_1.2.3_darwin_arm64.tar.gz", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write([]byte("tampered"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err = generate("1.2.3", "archives", "out"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
}
func TestStableVersion(t *testing.T) {
	for _, v := range []string{"", "v1.2.3", "1.2.3-dev", "../1.2.3", "1.02.3"} {
		if err := generate(v, "unused", "unused"); err == nil || !strings.Contains(err.Error(), "stable") {
			t.Fatalf("accepted %q: %v", v, err)
		}
	}
}
