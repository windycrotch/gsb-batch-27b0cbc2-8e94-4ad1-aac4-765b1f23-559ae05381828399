// peep-package creates release archives using only Go build tools.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
)

func run() error {
	platform := flag.String("os", runtime.GOOS, "target OS")
	arch := flag.String("arch", runtime.GOARCH, "target architecture")
	version := flag.String("version", "dev", "package version")
	output := flag.String("out", "dist", "output directory")
	flag.Parse()
	if (*platform != "linux" && *platform != "darwin") || (*arch != "arm64" && *arch != "amd64") {
		return fmt.Errorf("supported targets: macOS/Linux arm64/amd64")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(*version) {
		return fmt.Errorf("invalid version")
	}
	if err := os.MkdirAll(*output, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(*output, ".package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	binary := filepath.Join(stage, "peep")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+*version, "-o", binary, "./cmd/peep")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+*platform, "GOARCH="+*arch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return err
	}
	name := fmt.Sprintf("peep_%s_%s_%s.tar.gz", *version, *platform, *arch)
	stagedArchive := filepath.Join(stage, name)
	f, err := os.Create(stagedArchive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, hash))
	tw := tar.NewWriter(gz)
	files := []struct {
		from, to string
		mode     int64
	}{{binary, "bin/peep", 0755}, {"README.md", "README.md", 0644}, {"LICENSE", "LICENSE", 0644}, {"internal/diagram/notices/MMDG-LICENSE.txt", "licenses/MMDG-LICENSE.txt", 0644}, {"internal/diagram/notices/MMDG-NOTICES.txt", "licenses/MMDG-NOTICES.txt", 0644}}
	for _, file := range files {
		if err = add(tw, file.from, file.to, file.mode); err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			return err
		}
	}
	// Keep README links and the ready-to-browse demo usable after extraction.
	for _, root := range []string{"docs", "examples"} {
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(path), Linkname: target, Mode: 0777, Typeflag: tar.TypeSymlink})
			}
			return add(tw, path, filepath.ToSlash(path), 0644)
		})
		if err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			return err
		}
	}
	if err = tw.Close(); err != nil {
		f.Close()
		return err
	}
	if err = gz.Close(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	destination := filepath.Join(*output, name)
	if err = os.Rename(stagedArchive, destination); err != nil {
		return err
	}
	if err = os.WriteFile(destination+".sha256", []byte(fmt.Sprintf("%x  %s\n", hash.Sum(nil), name)), 0644); err != nil {
		return err
	}
	fmt.Println(destination)
	return nil
}
func add(tw *tar.Writer, source, name string, mode int64) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: st.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
