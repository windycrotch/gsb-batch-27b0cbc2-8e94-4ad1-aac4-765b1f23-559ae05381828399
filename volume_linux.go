//go:build !windows && !darwin

package volume

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

func cmdEnv() []string {
	return []string{"LANG=C", "LC_ALL=C"}
}

// newDefaultBackend returns the Linux audio backend. PulseAudio (pactl) is
// preferred; amixer is used as a fallback when pactl is unavailable. The
// availability is re-probed after every failure so that a transient outage
// never degrades the package to the wrong backend permanently.
func newDefaultBackend() audioBackend {
	pactl := &pactlBackend{}
	amixer := &amixerBackend{}
	return &failoverBackend{
		preferred: pactl,
		fallback:  amixer,
		probe: func() error {
			_, err := execCmd([]string{"pactl", "info"})
			return err
		},
	}
}

// pactlBackend controls the audio through PulseAudio's pactl command.
type pactlBackend struct{}

func (b *pactlBackend) Status() (State, error) {
	sinkName, sinkNameErr := getPulseAudioDefaultSink()
	out, err := execCmd([]string{"pactl", "list", "sinks"})
	if err != nil {
		return State{}, err
	}
	return parseState(string(out), false, sinkName, sinkNameErr)
}

func (b *pactlBackend) SetVolume(volume int) error {
	_, err := execCmd([]string{"pactl", "--", "set-sink-volume", "@DEFAULT_SINK@", strconv.Itoa(volume) + "%"})
	return err
}

func (b *pactlBackend) SetMuted(muted bool) error {
	flag := "0"
	if muted {
		flag = "1"
	}
	_, err := execCmd([]string{"pactl", "--", "set-sink-mute", "@DEFAULT_SINK@", flag})
	return err
}

// amixerBackend controls the audio through the ALSA amixer command.
type amixerBackend struct{}

func (b *amixerBackend) Status() (State, error) {
	out, err := execCmd([]string{"amixer", "get", "Master"})
	if err != nil {
		return State{}, err
	}
	return parseState(string(out), true, "", nil)
}

func (b *amixerBackend) SetVolume(volume int) error {
	_, err := execCmd([]string{"amixer", "set", "Master", strconv.Itoa(volume) + "%"})
	return err
}

func (b *amixerBackend) SetMuted(muted bool) error {
	action := "unmute"
	if muted {
		action = "mute"
	}
	_, err := execCmd([]string{"amixer", "-D", "pulse", "set", "Master", action})
	return err
}

func getPulseAudioDefaultSink() (string, error) {
	out, err := execCmd([]string{"pactl", "info"})
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(out), "\n")

	defaultSinkStr := "Default Sink: "
	for _, line := range lines {
		s := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(s, defaultSinkStr) {
			return strings.TrimSpace(strings.Replace(s, defaultSinkStr, "", 1)), nil
		}
	}
	return "", errors.New("could not find PulseAudio Default Sink")
}

var volumePattern = regexp.MustCompile(`\d+%`)

// parseState extracts the volume and the muted flag from a single command
// output, so both values describe the same point in time. sinkName locates
// the default PulseAudio sink; when sinkNameErr is non-nil the sink could
// not be identified, in which case the first sink in the output is used as
// a fallback rather than silently parsing the wrong sink. The isAmixer
// flag selects the ALSA output layout.
func parseState(out string, isAmixer bool, sinkName string, sinkNameErr error) (State, error) {
	// When the default sink name is known, only the block following its
	// "Name:" line is parsed. When it could not be determined, fall back to
	// the first sink block in the output.
	fallback := sinkNameErr != nil
	inDefaultBlock := fallback
	var state State
	volumeFound, mutedFound := false, false

	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimLeft(line, " \t")

		if !isAmixer && strings.HasPrefix(s, "Name: ") {
			inDefaultBlock = fallback || strings.Contains(s, "Name: "+sinkName)
		}

		amixerLine := isAmixer && strings.Contains(s, "Playback") && strings.Contains(s, "%")
		pactlVolumeLine := !isAmixer && inDefaultBlock && strings.HasPrefix(s, "Volume:")
		if !volumeFound && (amixerLine || pactlVolumeLine) {
			volumeStr := volumePattern.FindString(s)
			if volumeStr == "" {
				return State{}, errors.New("no volume found")
			}
			volume, err := strconv.Atoi(volumeStr[:len(volumeStr)-1])
			if err != nil {
				return State{}, err
			}
			state.Volume = volume
			volumeFound = true
		}

		pactlMuteLine := !isAmixer && inDefaultBlock && strings.HasPrefix(s, "Mute: ")
		if !mutedFound && (amixerLine || pactlMuteLine) {
			switch {
			case strings.Contains(s, "[off]") || strings.Contains(s, "yes"):
				state.Muted = true
				mutedFound = true
			case strings.Contains(s, "[on]") || strings.Contains(s, "no"):
				state.Muted = false
				mutedFound = true
			}
		}
	}

	if !volumeFound {
		return State{}, errors.New("no volume found")
	}
	if !mutedFound {
		return State{}, errors.New("no muted information found")
	}
	return state, nil
}
