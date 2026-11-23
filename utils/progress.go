package utils

import (
	"fmt"
	"sync"
	"time"
)

// Progress is a minimal, dependency-free terminal progress reporter.
// It is safe for concurrent use.
type Progress struct {
	total int

	mu       sync.Mutex
	count    int
	start    time.Time
	lastLog  time.Time
	finished bool
}

// StartProgress creates a Progress for a total of total units of work.
func StartProgress(total int) *Progress {
	if total < 1 {
		total = 1
	}
	now := time.Now()
	return &Progress{total: total, start: now, lastLog: now}
}

// Increment advances the progress by one unit, printing an update at most
// twice per second (and always at 100%).
func (p *Progress) Increment() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count < p.total {
		p.count++
	}
	if p.count == p.total || time.Since(p.lastLog) >= 500*time.Millisecond {
		fmt.Printf("\rProgress: %d/%d (%.1f%%)", p.count, p.total, 100*float64(p.count)/float64(p.total))
		p.lastLog = time.Now()
	}
}

// Finish prints the final progress line. Safe to call multiple times.
func (p *Progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	fmt.Printf("\rProgress: %d/%d (100.0%%) in %s\n", p.total, p.total, time.Since(p.start).Round(time.Millisecond))
}
