// Package fileops implements explicit local file operations without overwrites.
package fileops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Target struct {
	Path string
	Info os.FileInfo
}

func Capture(path string) (Target, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Target{}, err
	}
	if absolute == filepath.Dir(absolute) {
		return Target{}, errors.New("cannot modify a filesystem root")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Target{}, fmt.Errorf("inspect target: %w", err)
	}
	return Target{absolute, info}, nil
}
func (t Target) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.Info == nil || t.Path == "" || t.Path == filepath.Dir(t.Path) {
		return errors.New("invalid file target")
	}
	current, err := os.Lstat(t.Path)
	if err != nil {
		return fmt.Errorf("inspect target: %w", err)
	}
	if !os.SameFile(t.Info, current) {
		return errors.New("target changed since confirmation; select it again")
	}
	return nil
}

// Delete removes a confirmed target, including all contents of a directory.
// Symlinks are removed themselves, without following their targets.
func Delete(ctx context.Context, t Target) error {
	if err := t.validate(ctx); err != nil {
		return err
	}
	remove := os.Remove
	if t.Info.IsDir() {
		remove = os.RemoveAll
	}
	if err := remove(t.Path); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}
func Destination(t Target, input string, rename bool) (string, error) {
	if input == "" {
		return "", errors.New("enter a destination")
	}
	if rename {
		if input == "." || input == ".." || filepath.Base(input) != input {
			return "", errors.New("rename accepts a name only; use move for another directory")
		}
		return filepath.Join(filepath.Dir(t.Path), input), nil
	}
	if input == "~" || strings.HasPrefix(input, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if input == "~" {
			input = home
		} else {
			input = filepath.Join(home, input[2:])
		}
	}
	if !filepath.IsAbs(input) {
		input = filepath.Join(filepath.Dir(t.Path), input)
	}
	input = filepath.Clean(input)
	if st, err := os.Stat(input); err == nil && st.IsDir() {
		input = filepath.Join(input, filepath.Base(t.Path))
	}
	return input, nil
}
func Move(ctx context.Context, t Target, destination string) error {
	if err := t.validate(ctx); err != nil {
		return err
	}
	destination = filepath.Clean(destination)
	if !filepath.IsAbs(destination) {
		return errors.New("destination must be absolute")
	}
	if destination == t.Path {
		return errors.New("destination is the current path")
	}
	// The OS enforces no-replace atomically, including dangling destination symlinks.
	if err := renameNoReplace(t.Path, destination); err != nil {
		return fmt.Errorf("move without overwriting (same filesystem required): %w", err)
	}
	return nil
}
