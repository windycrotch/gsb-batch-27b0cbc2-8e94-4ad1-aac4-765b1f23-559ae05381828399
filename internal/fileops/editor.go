package fileops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// SplitEditor supports quoted executable paths and arguments, without invoking a
// shell. Substitution, pipelines, redirection and filename expansion never run.
func SplitEditor(value string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range value {
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("unclosed quote or escape in editor setting")
	}
	if started {
		args = append(args, word.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("editor command is empty")
	}
	return args, nil
}
func Editor(ctx context.Context, path string) (*exec.Cmd, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("editor requires a regular file")
	}
	setting := os.Getenv("VISUAL")
	if strings.TrimSpace(setting) == "" {
		setting = os.Getenv("EDITOR")
	}
	if strings.TrimSpace(setting) == "" {
		setting = "vi"
	}
	args, err := SplitEditor(setting)
	if err != nil {
		return nil, fmt.Errorf("configure VISUAL or EDITOR: %w", err)
	}
	args = append(args, absolute)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = filepath.Dir(absolute)
	return cmd, nil
}
