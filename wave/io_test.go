package wave

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/sqweek/sqribe/core/types"
)

// newTestCache builds a cache backed by a temp file with small blocks so a
// handful of samples spans several chunks. The cache is closed automatically at
// test end so its fetcher goroutine does not leak.
func newTestCache(t *testing.T, blocksz uint) (*cache, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "cache.bin")
	c := mkcache(blocksz, 2, file)
	t.Cleanup(func() { c.Close() })
	return c, file
}

// writeCacheFile lays down raw little-endian int16 samples.
func writeCacheFile(t *testing.T, path string, samples []int16) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := binary.Write(f, binary.LittleEndian, samples); err != nil {
		t.Fatal(err)
	}
}

// TestCacheWaitReturnsOnCancel ensures a wait for a chunk that never arrives is
// interruptible instead of blocking forever.
func TestCacheWaitReturnsOnCancel(t *testing.T) {
	c, _ := newTestCache(t, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.wait(ctx, 42)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait did not return promptly on cancellation")
	}
}

// TestCacheEOF verifies that chunks past the end of a fully decoded stream are
// reported as ErrCacheEOF rather than waited on indefinitely.
func TestCacheEOF(t *testing.T) {
	c, file := newTestCache(t, 16)
	// 40 bytes => last chunk id 2, with 8 bytes in it
	samples := make([]int16, 20)
	writeCacheFile(t, file, samples)
	c.mu.Lock()
	c.bytesWritten = 40
	c.lastChunkId = 2
	c.lastChunkSize = 8
	c.bytesWritten = -1
	c.mu.Unlock()

	ctx := context.Background()
	if _, err := c.wait(ctx, 99); !errors.Is(err, ErrCacheEOF) {
		t.Fatalf("want ErrCacheEOF, got %v", err)
	}
}

// TestCacheReadFailure marks a chunk as failed and asserts wait surfaces
// ErrCacheUnavailable.
func TestCacheReadFailure(t *testing.T) {
	c, _ := newTestCache(t, 16)
	c.add(7, failure)
	if _, err := c.wait(context.Background(), 7); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("want ErrCacheUnavailable, got %v", err)
	}
}

// TestFramesCtxEndToEnd writes a small cache file, lets the fetcher load chunks
// and reads them back through the Waveform entry point with a live context.
func TestFramesCtxEndToEnd(t *testing.T) {
	c, file := newTestCache(t, 32)
	n := 64 // samples; 32 frames of stereo
	samples := make([]int16, n)
	for i := range samples {
		samples[i] = int16(i - 32)
	}
	writeCacheFile(t, file, samples)
	c.mu.Lock()
	c.bytesWritten = int64(n * 2)
	c.lastChunkId = uint64((n*2 - 1) / 32)
	c.lastChunkSize = uint((n * 2) % 32)
	if c.lastChunkSize == 0 {
		c.lastChunkSize = 32
	}
	c.bytesWritten = -1
	c.mu.Unlock()

	wav := &Waveform{Channels: 2, rate: 44100, cache: c}
	got, err := wav.FramesCtx(context.Background(), 0, 3)
	if err != nil {
		t.Fatalf("FramesCtx: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("got %d samples, want 8", len(got))
	}
	for i, v := range got {
		if v != samples[i] {
			t.Fatalf("sample %d = %d, want %d", i, v, samples[i])
		}
	}
}

// TestFramesCtxPastEOF asks for frames beyond the decoded length and expects an
// error rather than a crash or infinite wait.
func TestFramesCtxPastEOF(t *testing.T) {
	c, file := newTestCache(t, 16)
	samples := make([]int16, 8) // 4 stereo frames
	writeCacheFile(t, file, samples)
	c.mu.Lock()
	c.bytesWritten = 16
	c.lastChunkId = 0
	c.lastChunkSize = 16
	c.bytesWritten = -1
	c.mu.Unlock()

	wav := &Waveform{Channels: 2, rate: 44100, cache: c}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := wav.FramesCtx(ctx, 100, 200); err == nil {
		t.Fatal("expected error reading past EOF, got nil")
	}
}

// TestCacheConcurrentWaits hammers wait/add from many goroutines to validate
// the locking under -race.
func TestCacheConcurrentWaits(t *testing.T) {
	c, file := newTestCache(t, 32)
	samples := make([]int16, 256)
	writeCacheFile(t, file, samples)

	var wg sync.WaitGroup
	// concurrent readers of not-yet-ready chunks
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = c.wait(ctx, 0)
		}()
	}
	// mark decoding complete so the fetcher can satisfy the waits
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	c.bytesWritten = int64(len(samples) * 2)
	c.lastChunkId = uint64(len(samples)*2/32) - 1
	c.lastChunkSize = 32
	c.bytesWritten = -1
	c.mu.Unlock()
	// prompt the fetcher
	c.iochan <- 0
	wg.Wait()
}

// TestWaveformNSamplesAtomic exercises the atomic sample counter alongside
// reads.
func TestWaveformNSamplesAtomic(t *testing.T) {
	wav := &Waveform{Channels: 2}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 1000; k++ {
				wav.addSamples(2)
				_ = wav.NSamplesCount()
			}
		}()
	}
	wg.Wait()
	if got := wav.NSamplesCount(); got != SampleN(8000) {
		t.Fatalf("NSamples = %d, want 8000", got)
	}
}
