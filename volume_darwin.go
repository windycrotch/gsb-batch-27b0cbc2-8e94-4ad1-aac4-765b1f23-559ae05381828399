package volume

import (
	"fmt"
	"strconv"
	"strings"
)

func cmdEnv() []string {
	return nil
}

// newDefaultBackend returns the macOS audio backend driven by osascript.
func newDefaultBackend() audioBackend {
	return &darwinBackend{}
}

type darwinBackend struct{}

func (b *darwinBackend) Status() (State, error) {
	out, err := execCmd([]string{"osascript", "-e", "output volume of (get volume settings)"})
	if err != nil {
		return State{}, err
	}
	volume, err := parseVolume(string(out))
	if err != nil {
		return State{}, err
	}
	out, err = execCmd([]string{"osascript", "-e", "output muted of (get volume settings)"})
	if err != nil {
		return State{}, err
	}
	muted, err := parseMuted(string(out))
	if err != nil {
		return State{}, err
	}
	return State{Volume: volume, Muted: muted}, nil
}

func (b *darwinBackend) SetVolume(volume int) error {
	_, err := execCmd([]string{"osascript", "-e", "set volume output volume " + strconv.Itoa(volume)})
	return err
}

func (b *darwinBackend) SetMuted(muted bool) error {
	flag := "false"
	if muted {
		flag = "true"
	}
	_, err := execCmd([]string{"osascript", "-e", "set volume output muted " + flag})
	return err
}

func parseVolume(out string) (int, error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "missing value" {
		return 0, fmt.Errorf("failed to get volume settings: %s", out)
	}
	return strconv.Atoi(out)
}

func parseMuted(out string) (bool, error) {
	switch strings.TrimSpace(out) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("unknown muted status: %s", out)
}
