package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionIdentity(t *testing.T) {
	git := &debug.BuildInfo{Main: debug.Module{Version: "v0.2.2-0.20261123075328-0123456789ab+dirty"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "true"},
	}}
	for _, tc := range []struct {
		name, release string
		info          *debug.BuildInfo
		want          string
	}{
		{"release overrides metadata", "0.2.1", git, "0.2.1"},
		{"modified checkout", "dev", git, "dev (0123456789ab, dirty)"},
		{"clean checkout", "dev", &debug.BuildInfo{Settings: git.Settings[:1]}, "dev (0123456789ab)"},
		{"short revision", "dev", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}}, "dev (abc)"},
		{"module install", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.2.1"}}, "v0.2.1"},
		{"no VCS", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev (revision unavailable)"},
		{"no build info", "dev", nil, "dev (revision unavailable)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVersion(tc.release, tc.info); got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
		})
	}
}
