// Package observatory keeps the night's signals in order.
package observatory

import "time"

type Recording struct {
	Name     string
	Captured time.Time
	Duration time.Duration
	Checksum string
}

// Keep preserves a complete signal for the next shift.
func Keep(signal Recording) bool {
	return signal.Duration > 0 && signal.Checksum != ""
}

func NightShift() []string {
	return []string{"observe", "capture", "listen", "archive"}
}
