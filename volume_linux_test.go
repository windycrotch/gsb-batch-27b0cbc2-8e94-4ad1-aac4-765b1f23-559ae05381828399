//go:build !windows && !darwin

package volume

import "testing"

const fakePactlSinks = `Sink #0
	State: RUNNING
	Name: SOME_OTHER_SINK
	Volume: 0: 99%
	Mute: no

Sink #1
	State: RUNNING
	Name: DEFAULT_SINK
	Volume: 0: 37%
	Mute: no
`

func TestParseStatePactlDefaultSink(t *testing.T) {
	state, err := parseState(fakePactlSinks, false, "DEFAULT_SINK", nil)
	if err != nil {
		t.Fatalf("parseState failed: %+v", err)
	}
	if state.Volume != 37 {
		t.Errorf("default sink volume failed: (got: %d, expected: 37)", state.Volume)
	}
	if state.Muted {
		t.Errorf("default sink should not be muted")
	}
}

func TestParseStatePactlMuted(t *testing.T) {
	out := `Sink #1
	State: RUNNING
	Name: DEFAULT_SINK
	Volume: 0: 81%
	Mute: yes
`
	state, err := parseState(out, false, "DEFAULT_SINK", nil)
	if err != nil {
		t.Fatalf("parseState failed: %+v", err)
	}
	if state.Volume != 81 || !state.Muted {
		t.Errorf("muted state failed: (got: %+v, expected volume 81 and muted)", state)
	}
}

func TestParseStateAmixer(t *testing.T) {
	out := `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: 0 - 255
  Front Left: Playback 153 [60%] [on]
  Front Right: Playback 153 [60%] [on]
`
	state, err := parseState(out, true, "", nil)
	if err != nil {
		t.Fatalf("parseState failed: %+v", err)
	}
	if state.Volume != 60 || state.Muted {
		t.Errorf("amixer state failed: (got: %+v, expected volume 60 and unmuted)", state)
	}
}

func TestParseStateAmixerMuted(t *testing.T) {
	out := `Simple mixer control 'Master',0
  Front Left: Playback 0 [0%] [off]
  Front Right: Playback 0 [0%] [off]
`
	state, err := parseState(out, true, "", nil)
	if err != nil {
		t.Fatalf("parseState failed: %+v", err)
	}
	if state.Volume != 0 || !state.Muted {
		t.Errorf("amixer muted state failed: (got: %+v)", state)
	}
}

func TestParseStateErrors(t *testing.T) {
	if _, err := parseState("Name: DEFAULT_SINK\n", false, "DEFAULT_SINK", nil); err == nil {
		t.Errorf("missing volume should be an error")
	}
	if _, err := parseState("", false, "DEFAULT_SINK", nil); err == nil {
		t.Errorf("empty output should be an error")
	}
}
