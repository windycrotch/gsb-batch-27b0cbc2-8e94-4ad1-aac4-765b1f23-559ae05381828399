// Package installation installs peep as one versioned, self-contained executable.
package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Install verifies a staged copy before atomically replacing the public symlink.
// Previous versions remain usable by running peep processes and their workers.
func Install(ctx context.Context, source, prefix string) (string, error) {
	return InstallTo(ctx, source, prefix, "")
}

// InstallTo optionally places the public executable in a separate bin directory.
// This preserves legacy Go install locations while keeping versioned storage.
func InstallTo(ctx context.Context, source, prefix, bin string) (string, error) {
	prefix, err := filepath.Abs(prefix)
	if err != nil {
		return "", err
	}
	if bin == "" {
		bin = filepath.Join(prefix, "bin")
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return "", err
	}
	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open peep executable: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("peep executable is not a regular file")
	}
	versions := filepath.Join(prefix, "lib", "peep", "versions")
	if err = os.MkdirAll(versions, 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(versions, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "peep")
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, digest), contextReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("stage peep: %w", err)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if err = verify(ctx, staged, sum); err != nil {
		return "", err
	}
	target := filepath.Join(versions, sum)
	if _, err = os.Lstat(target); os.IsNotExist(err) {
		if err = os.Rename(stage, target); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if err = verify(ctx, filepath.Join(target, "peep"), sum); err != nil {
		return "", fmt.Errorf("existing version damaged: %w", err)
	}
	if err = os.MkdirAll(bin, 0755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(bin, ".peep-link-")
	if err != nil {
		return "", err
	}
	link := temp.Name()
	if err = temp.Close(); err != nil {
		os.Remove(link)
		return "", err
	}
	defer os.Remove(link)
	if err = os.Remove(link); err != nil {
		return "", err
	}
	if err = os.Symlink(filepath.Join(target, "peep"), link); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	public := filepath.Join(bin, "peep")
	if err = os.Rename(link, public); err != nil {
		return "", fmt.Errorf("activate peep: %w", err)
	}
	return public, nil
}
func verify(ctx context.Context, path, want string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("%s is not a regular executable", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("executable checksum mismatch")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
