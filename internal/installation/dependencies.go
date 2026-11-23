package installation

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

func EnsureMedia(ctx context.Context, in io.Reader, out, stderr io.Writer) error {
	missing := false
	for _, name := range []string{"ffmpeg", "ffprobe", "mpv"} {
		if _, err := exec.LookPath(name); err != nil {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	plan, err := dependencyPlan(runtime.GOOS, os.Geteuid() == 0, exec.LookPath)
	if err != nil {
		return err
	}
	for _, args := range plan {
		fmt.Fprintf(out, "Installing media dependencies: %v\n", args)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = in
		cmd.Stdout = out
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install media dependencies: %w", err)
		}
	}
	for _, name := range []string{"ffmpeg", "ffprobe", "mpv"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s is still missing after dependency installation", name)
		}
	}
	return nil
}
func dependencyPlan(platform string, root bool, lookup func(string) (string, error)) ([][]string, error) {
	if platform == "darwin" {
		if path, err := lookup("brew"); err == nil {
			return [][]string{{path, "install", "ffmpeg", "mpv"}}, nil
		}
		return nil, fmt.Errorf("Homebrew is required to install media tools; install ffmpeg/mpv or use --no-deps")
	}
	if platform != "linux" {
		return nil, fmt.Errorf("automatic media setup is unavailable on %s; use --no-deps", platform)
	}
	for _, candidate := range []struct {
		name string
		args []string
	}{{"apt-get", []string{"install", "-y", "ffmpeg", "mpv"}}, {"dnf", []string{"install", "-y", "ffmpeg", "mpv"}}, {"pacman", []string{"-S", "--needed", "--noconfirm", "ffmpeg", "mpv"}}} {
		path, err := lookup(candidate.name)
		if err != nil {
			continue
		}
		command := append([]string{path}, candidate.args...)
		if !root {
			sudo, err := lookup("sudo")
			if err != nil {
				return nil, fmt.Errorf("sudo is required to install media tools; install them manually or use --no-deps")
			}
			command = append([]string{sudo}, command...)
		}
		return [][]string{command}, nil
	}
	return nil, fmt.Errorf("no supported package manager found; install ffmpeg/mpv or use --no-deps")
}
