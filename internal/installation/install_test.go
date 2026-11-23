package installation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestInstallRunningExecutable(t *testing.T) {
	if os.Getenv("PEEP_INSTALL_TEST_CHILD") == "1" {
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(bin, "peep")
	if err := os.WriteFile(public, data, 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, public, "-test.run=^TestInstallRunningExecutable$")
	cmd.Env = append(os.Environ(), "PEEP_INSTALL_TEST_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Error("old executable failed after upgrade:", err)
		}
	}()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child startup: %q %v", line, err)
	}
	source := filepath.Join(dir, "new")
	if err := os.WriteFile(source, []byte("replacement"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallTo(ctx, source, filepath.Join(dir, "prefix"), bin); err != nil {
		t.Fatal("could not replace a running executable:", err)
	}
	data, err = os.ReadFile(public)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement missing: %s %v", data, err)
	}
}

func TestInstallUpgradeAndFailurePreservation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "peep")
	prefix := filepath.Join(dir, "prefix with spaces")
	if err := os.WriteFile(source, []byte("first version"), 0755); err != nil {
		t.Fatal(err)
	}
	public, err := Install(context.Background(), source, prefix)
	if err != nil {
		t.Fatal(err)
	}
	first, err := filepath.EvalSymlinks(public)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte("second version"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = Install(context.Background(), source, prefix); err != nil {
		t.Fatal(err)
	}
	second, _ := filepath.EvalSymlinks(public)
	if second == first {
		t.Fatal("upgrade did not switch version")
	}
	data, _ := os.ReadFile(first)
	if string(data) != "first version" {
		t.Fatal("running version removed")
	}
	data, _ = os.ReadFile(public)
	if string(data) != "second version" {
		t.Fatal("wrong installed contents")
	}
	if _, err = Install(context.Background(), source, prefix); err != nil {
		t.Fatal("repeat install", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Install(ctx, source, prefix); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
	after, _ := filepath.EvalSymlinks(public)
	if after != second {
		t.Fatal("failed install changed active version")
	}
	if _, err = Install(context.Background(), filepath.Join(dir, "missing"), prefix); err == nil {
		t.Fatal("missing source accepted")
	}
	if err = os.WriteFile(second, []byte("corrupt existing copy"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = Install(context.Background(), source, prefix); err == nil {
		t.Fatal("damaged version accepted")
	}
	entries, _ := filepath.Glob(filepath.Join(prefix, "lib/peep/versions/.stage-*"))
	if len(entries) > 0 {
		t.Fatal("staging leaked", entries)
	}
}
func TestFailedActivationPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	os.WriteFile(source, []byte("binary"), 0755)
	public := filepath.Join(dir, "install/bin/peep")
	os.MkdirAll(public, 0755)
	os.WriteFile(filepath.Join(public, "keep"), []byte("safe"), 0600)
	if _, err := Install(context.Background(), source, filepath.Join(dir, "install")); err == nil {
		t.Fatal("replaced directory")
	}
	if _, err := os.Stat(filepath.Join(public, "keep")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallReplacesLegacyExecutableWithoutFollowingLinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint("symlink=", symlink), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "new")
			old := filepath.Join(dir, "old")
			bin := filepath.Join(dir, "custom go executables")
			public := filepath.Join(bin, "peep")
			for path, content := range map[string]string{source: "new", old: "old"} {
				if err := os.WriteFile(path, []byte(content), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(old, public); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(public, []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			opened, err := os.Open(public)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if got, err := InstallTo(context.Background(), source, filepath.Join(dir, "prefix"), bin); err != nil || got != public {
				t.Fatalf("install: %q %v", got, err)
			}
			data, err := os.ReadFile(public)
			if err != nil || string(data) != "new" {
				t.Fatalf("old command not replaced: %s %v", data, err)
			}
			data, err = io.ReadAll(opened)
			if err != nil || string(data) != "old" {
				t.Fatalf("open old executable changed: %s %v", data, err)
			}
			data, err = os.ReadFile(old)
			if err != nil || string(data) != "old" {
				t.Fatalf("symlink target changed: %s %v", data, err)
			}
		})
	}
}
func TestDependencyPlans(t *testing.T) {
	for _, tc := range []struct {
		platform string
		root     bool
		tools    []string
		want     []string
	}{
		{"darwin", false, []string{"brew"}, []string{"/tools/brew", "install", "ffmpeg", "mpv"}},
		{"linux", false, []string{"apt-get", "sudo"}, []string{"/tools/sudo", "/tools/apt-get", "install", "-y", "ffmpeg", "mpv"}},
		{"linux", true, []string{"pacman"}, []string{"/tools/pacman", "-S", "--needed", "--noconfirm", "ffmpeg", "mpv"}},
	} {
		lookup := func(name string) (string, error) {
			for _, tool := range tc.tools {
				if name == tool {
					return "/tools/" + name, nil
				}
			}
			return "", os.ErrNotExist
		}
		plan, err := dependencyPlan(tc.platform, tc.root, lookup)
		if err != nil || len(plan) != 1 || !reflect.DeepEqual(plan[0], tc.want) {
			t.Fatal(plan, err)
		}
	}
	if _, err := dependencyPlan("linux", false, func(string) (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Fatal("missing manager accepted")
	}
}
