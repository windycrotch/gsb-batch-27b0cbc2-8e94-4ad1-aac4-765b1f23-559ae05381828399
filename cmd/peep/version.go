package main

import "runtime/debug"

func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return formatVersion(version, info)
}

// Read the build's embedded revision, never the working directory at runtime.
func formatVersion(release string, info *debug.BuildInfo) string {
	if release != "" && release != "dev" {
		return release
	}
	if info != nil {
		var revision string
		var dirty bool
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
		if revision != "" {
			if len(revision) > 12 {
				revision = revision[:12]
			}
			if dirty {
				revision += ", dirty"
			}
			return "dev (" + revision + ")"
		}
		// go install module@version supplies a module version without Git metadata.
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev (revision unavailable)"
}
