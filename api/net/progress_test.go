package net

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProgressReporterStopsBeforeReturning verifies that the returned stop
// function blocks until the reporter goroutine exited, so no progress write can
// happen after a transfer stage has finished.
func TestProgressReporterStopsBeforeReturning(t *testing.T) {
	var reports int64
	report := func(p float64) {
		atomic.AddInt64(&reports, 1)
	}

	stop := startProgressReporter(context.Background(), 5*time.Millisecond, func() float64 { return 42 }, report)
	waitForCount(t, &reports, 1)
	stop()

	// After stop returns, the number of reports must not change anymore.
	after := atomic.LoadInt64(&reports)
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt64(&reports); got != after {
		t.Fatalf("progress reported after stop: %d -> %d", after, got)
	}
}

// TestProgressReporterHonoursContext verifies that a cancelled transfer stops
// reporting on its own.
func TestProgressReporterHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var reports int64
	stop := startProgressReporter(ctx, 5*time.Millisecond, func() float64 { return 42 }, func(float64) {
		atomic.AddInt64(&reports, 1)
	})
	defer stop()

	waitForCount(t, &reports, 1)
	cancel()

	// Once cancelled the reporter must stop; give it a tick to observe it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		before := atomic.LoadInt64(&reports)
		time.Sleep(60 * time.Millisecond)
		if atomic.LoadInt64(&reports) == before {
			return
		}
	}
	t.Fatal("progress kept being reported after context cancellation")
}

func TestStartProgressReporterWithoutCallback(t *testing.T) {
	// A nil reporter must be safe and return an immediately usable stop func.
	stop := startProgressReporter(context.Background(), 5*time.Millisecond, func() float64 { return 0 }, nil)
	stop()
}

// TestStartProgressReporterSerialisesReads guards against data races between the
// reporter reading the counters and the transfer updating them.
func TestStartProgressReporterSerialisesReads(t *testing.T) {
	var mu sync.Mutex
	total := 0.0
	stop := startProgressReporter(context.Background(), 5*time.Millisecond, func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return total
	}, func(float64) {})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			mu.Lock()
			total += 1
			mu.Unlock()
		}
	}()
	wg.Wait()
	stop()
}

func waitForCount(t *testing.T, n *int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(n) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d progress reports", want)
}
