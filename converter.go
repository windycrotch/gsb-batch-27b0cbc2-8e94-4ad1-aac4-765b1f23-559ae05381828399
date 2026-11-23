package main

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	converterQueueSize      = 100
	converterMaxAttempts    = 5
	converterBaseBackoff    = 5 * time.Second
	converterMaxBackoff     = 5 * time.Minute
	converterRescanInterval = 10 * time.Second
)

// Converter converts recorded .ts segments from the .tmp directory into
// their final, dated directory using ffmpeg.
//
// Guarantees:
//   - a failed conversion is retried with an increasing backoff; after
//     maxAttempts the segment is marked as failed, automatic processing
//     stops for it and the source segment is kept for manual recovery;
//   - the final file is written to a temporary name and atomically
//     renamed into place, and the source segment is only removed after
//     the final file is completely on disk — a half-written file never
//     appears in the final directory;
//   - if the final file already exists and is not empty, the segment is
//     considered done: the source is removed without converting again;
//   - the same path can be submitted any number of times (recording
//     finished, startup scan, ...) but only one conversion task runs;
//   - on shutdown an in-progress conversion is interrupted without
//     deleting the source, and queued segments stay in .tmp untouched.
type Converter struct {
	videosDir string
	ffmpeg    string

	queue          chan string
	maxAttempts    int
	baseBackoff    time.Duration
	maxBackoff     time.Duration
	rescanInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	inflight map[string]struct{}
	failed   map[string]struct{}

	wg sync.WaitGroup

	// runCmd executes ffmpeg. It is a field so tests can stub it.
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func NewConverter(videosDir, ffmpegPath string) *Converter {
	ctx, cancel := context.WithCancel(context.Background())
	return &Converter{
		videosDir:      videosDir,
		ffmpeg:         ffmpegPath,
		queue:          make(chan string, converterQueueSize),
		maxAttempts:    converterMaxAttempts,
		baseBackoff:    converterBaseBackoff,
		maxBackoff:     converterMaxBackoff,
		rescanInterval: converterRescanInterval,
		ctx:            ctx,
		cancel:         cancel,
		inflight:       make(map[string]struct{}),
		failed:         make(map[string]struct{}),
		runCmd:         defaultRunCmd,
	}
}

func defaultRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Start launches the conversion worker.
func (c *Converter) Start() {
	c.wg.Add(1)
	go c.worker()
}

// Stop interrupts any in-progress conversion (keeping its source
// segment) and waits for the worker and the orphan scanner to exit.
// Segments still queued remain in .tmp for the next process start.
func (c *Converter) Stop() {
	c.cancel()
	c.wg.Wait()
}

// normalizePath canonicalizes a segment path so the same file is
// recognized regardless of how the path was built (path.Join by the
// recorder vs. filepath.Walk by the scanner differ on Windows).
func normalizePath(p string) string {
	return filepath.Clean(p)
}

// Submit enqueues a .ts segment for conversion. It never blocks and is
// safe for concurrent use. It reports whether the segment is (or will
// be) handled: true when newly queued, already in flight, or already
// marked as failed; false only when the queue is momentarily full and
// the caller should try again later.
func (c *Converter) Submit(tsFile string) bool {
	tsFile = normalizePath(tsFile)

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.failed[tsFile]; ok {
		// Already gave up on this segment; leave it for manual recovery.
		return true
	}
	if _, ok := c.inflight[tsFile]; ok {
		// Duplicate submission; the queued/running task covers it.
		return true
	}

	select {
	case c.queue <- tsFile:
		c.inflight[tsFile] = struct{}{}
		return true
	default:
		return false
	}
}

func (c *Converter) forget(tsFile string) {
	c.mu.Lock()
	delete(c.inflight, tsFile)
	c.mu.Unlock()
}

func (c *Converter) markFailed(tsFile string) {
	c.mu.Lock()
	c.failed[tsFile] = struct{}{}
	c.mu.Unlock()
}

func (c *Converter) isFailed(tsFile string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.failed[normalizePath(tsFile)]
	return ok
}

func (c *Converter) isInflight(tsFile string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.inflight[normalizePath(tsFile)]
	return ok
}

func (c *Converter) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case tsFile := <-c.queue:
			c.process(tsFile)
		}
	}
}

// finalPathFor maps a .tmp segment to its final directory and path.
// Filename format: rec_YYYY_MM_DD-HH_MM_SS_camera.ext.ts
func (c *Converter) finalPathFor(tsFile string) (finalDir, finalPath string, ok bool) {
	fileName := filepath.Base(tsFile)
	parts := strings.SplitN(fileName, "-", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	dayDir := parts[0] // rec_YYYY_MM_DD
	rest := parts[1]   // HH_MM_SS_camera.ext.ts

	finalFileName := strings.TrimSuffix(rest, ".ts")
	finalDir = path.Join(c.videosDir, dayDir)
	return finalDir, path.Join(finalDir, finalFileName), true
}

func (c *Converter) process(tsFile string) {
	defer c.forget(tsFile)

	if !strings.HasSuffix(tsFile, ".ts") {
		return
	}

	if _, err := os.Stat(tsFile); err != nil {
		// Source is gone (e.g. already handled); nothing to do.
		return
	}

	finalDir, finalFilePath, ok := c.finalPathFor(tsFile)
	if !ok {
		logger.Printf("cannot parse segment file name %s, skipping", tsFile)
		return
	}

	// Already converted in a previous run: clean up the source instead
	// of converting the same segment twice.
	if info, err := os.Stat(finalFilePath); err == nil && info.Size() > 0 {
		logger.Printf("final file %s already exists, removing source %s", finalFilePath, tsFile)
		if err := os.Remove(tsFile); err != nil {
			logger.Printf("error removing original ts file %s: %s", tsFile, err)
		}
		return
	}

	// The final file is first written to a temporary name in the same
	// directory and atomically renamed into place once complete, so a
	// half-written file never appears in the final directory.
	tmpFinal := finalFilePath + ".part"

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if c.ctx.Err() != nil {
			// Shutting down: leave the source segment in .tmp untouched.
			return
		}

		if err := os.MkdirAll(finalDir, 0774); err != nil {
			lastErr = err
			logger.Printf("error creating final directory %s: %s", finalDir, err)
		} else {
			logger.Printf("converting %s to %s (attempt %d/%d)", tsFile, finalFilePath, attempt, c.maxAttempts)

			args := []string{"-y", "-i", tsFile, "-c", "copy"}
			if strings.ToLower(filepath.Ext(finalFilePath)) == ".mp4" {
				args = append(args, "-movflags", "+faststart")
			}
			args = append(args, tmpFinal)

			out, err := c.runCmd(c.ctx, c.ffmpeg, args...)
			if err == nil {
				err = os.Rename(tmpFinal, finalFilePath)
			}

			if err == nil {
				logger.Printf("conversion finished: %s", finalFilePath)
				// The final file is completely on disk; only now the
				// source segment may be removed.
				if err := os.Remove(tsFile); err != nil {
					logger.Printf("error removing original ts file %s: %s", tsFile, err)
				}
				return
			}

			lastErr = err
			if len(out) > 0 {
				logger.Printf("conversion attempt %d/%d for %s failed: %s\nOutput: %s",
					attempt, c.maxAttempts, tsFile, err, string(out))
			} else {
				logger.Printf("conversion attempt %d/%d for %s failed: %s",
					attempt, c.maxAttempts, tsFile, err)
			}
		}

		// Never leave a partial file behind.
		os.Remove(tmpFinal)

		if c.ctx.Err() != nil {
			// Interrupted by shutdown: the source segment stays in .tmp
			// and the segment is NOT marked as failed.
			logger.Printf("conversion of %s interrupted by shutdown, source kept", tsFile)
			return
		}

		if attempt < c.maxAttempts {
			backoff := c.baseBackoff << (attempt - 1)
			if backoff > c.maxBackoff {
				backoff = c.maxBackoff
			}
			select {
			case <-c.ctx.Done():
				logger.Printf("conversion of %s interrupted by shutdown, source kept", tsFile)
				return
			case <-time.After(backoff):
			}
		}
	}

	c.markFailed(tsFile)
	logger.Printf("giving up on %s after %d attempts, source segment kept for manual recovery: %s",
		tsFile, c.maxAttempts, lastErr)
}

// ScanOrphans looks for leftover .ts segments in .tmp and enqueues them.
// If the queue is full the scan waits and retries, so a full queue never
// permanently blocks orphan recovery. It stops when the converter stops.
func (c *Converter) ScanOrphans() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			pending := c.findOrphans()
			if len(pending) == 0 {
				return
			}
			for _, p := range pending {
				logger.Printf("found orphaned file: %s", p)
				c.Submit(p)
			}
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(c.rescanInterval):
			}
		}
	}()
}

// findOrphans lists .ts segments in .tmp that are neither in flight nor
// marked as failed.
func (c *Converter) findOrphans() []string {
	tmpDir := path.Join(c.videosDir, ".tmp")
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		return nil
	}

	var pending []string
	err := filepath.Walk(tmpDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".ts") &&
			!c.isInflight(normalizePath(p)) && !c.isFailed(normalizePath(p)) {
			pending = append(pending, p)
		}
		return nil
	})
	if err != nil {
		logger.Printf("error scanning for existing .ts files in .tmp: %s", err)
	}
	return pending
}

// defaultConverter is the process-wide converter used by the recorder
// and main.
var defaultConverter *Converter

func StartConverter() {
	defaultConverter = NewConverter(videosDir, ffmpeg)
	defaultConverter.Start()
}

// StopConverter interrupts the in-progress conversion (if any) without
// deleting its source segment and leaves queued segments in .tmp.
func StopConverter() {
	if defaultConverter != nil {
		defaultConverter.Stop()
	}
}

func ScanExistingFiles() {
	defaultConverter.ScanOrphans()
}

// EnqueueConversion submits a freshly recorded segment to the converter.
// It never blocks; if the queue is momentarily full the periodic orphan
// scan picks the segment up from .tmp later.
func EnqueueConversion(tsFile string) {
	if defaultConverter == nil {
		return
	}
	defaultConverter.Submit(tsFile)
}
