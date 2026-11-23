// Package uploadertest provides an in-memory Uploader implementation used
// by the reliability tests. It behaves like a remote origin that can be
// configured to fail transiently, and it records every operation so tests
// can assert ordering invariants (for example that a chunklist never
// references a chunk that was not actually available at publish time).
package uploadertest

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
)

// Event records one observed upload operation.
type Event struct {
	// Kind is "upload" (chunk/object) or "manifest".
	Kind string
	// Path is the destination object path.
	Path string
	// ReferencedURIs holds the segment URIs referenced by a manifest event
	// (empty for non-manifest events).
	ReferencedURIs []string
	// AvailableObjects is a snapshot of every object already present at the
	// destination when the event happened. It lets tests prove a manifest
	// never referenced an unavailable object.
	AvailableObjects []string
}

// FakeUploader is an in-memory origin that mimics the retry contract of
// the real HTTP/S3 uploaders: retryable failures are retried internally a
// bounded number of times with linear backoff, permanent failures abort
// immediately, and exhaustion wraps uploaders.ErrRetriesExhausted.
type FakeUploader struct {
	mu sync.Mutex

	// Objects holds the current state of the "origin".
	Objects map[string][]byte

	// Events is the ordered operation log (successful publications only).
	Events []Event

	// callCount tracks attempts (including failed retries) per path.
	callCount map[string]int

	// MaxRetries bounds the attempts for one object. Defaults to 10 when 0.
	MaxRetries int
	// RetryDelay is the base backoff; delay before attempt N is N*RetryDelay.
	RetryDelay time.Duration

	// RetryableFailures is the number of upcoming attempts that fail with a
	// retryable error before succeeding. Each failed attempt consumes one.
	RetryableFailures int32

	// FailForeverRetryable makes every attempt fail with a retryable error;
	// used to exercise the retries-exhausted path.
	FailForeverRetryable bool

	// PermanentFailure, when non-nil, is returned immediately and must not
	// be retried by the uploader wrappers.
	PermanentFailure error

	// ChunkedTransferError makes the chunked transfer error channel report
	// this error instead of nil.
	ChunkedTransferError error
}

// NewFakeUploader builds an empty fake origin.
func NewFakeUploader() *FakeUploader {
	return &FakeUploader{
		Objects:    make(map[string][]byte),
		callCount:  make(map[string]int),
		MaxRetries: 10,
		RetryDelay: time.Millisecond,
	}
}

// Compile-time interface checks.
var (
	_ uploaders.Uploader                = (*FakeUploader)(nil)
	_ uploaders.ChunkedTransferUploader = (*FakeUploader)(nil)
)

func (f *FakeUploader) maxRetries() int {
	if f.MaxRetries < 1 {
		return 10
	}
	return f.MaxRetries
}

func (f *FakeUploader) snapshotObjects() []string {
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	return keys
}

func (f *FakeUploader) record(kind, path string, data []byte) {
	ev := Event{
		Kind:             kind,
		Path:             path,
		AvailableObjects: f.snapshotObjects(),
	}
	if kind == "manifest" {
		ev.ReferencedURIs = extractURIs(data)
	}
	f.Events = append(f.Events, ev)
}

// extractURIs returns the non-tag lines (segment URIs) of an HLS manifest.
func extractURIs(manifest []byte) []string {
	uris := make([]string, 0)
	for _, line := range strings.Split(string(manifest), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
		}
	}
	return uris
}

// classifyFailure consumes/returns a configured failure without touching
// the stored state, so a failed attempt never creates a partial object.
func (f *FakeUploader) classifyFailure() error {
	if f.PermanentFailure != nil {
		return fmt.Errorf("%w: %v", uploaders.ErrPermanentUploadError, f.PermanentFailure)
	}
	if f.FailForeverRetryable {
		return fmt.Errorf("fake origin temporarily unavailable")
	}
	if f.RetryableFailures > 0 {
		f.RetryableFailures--
		return fmt.Errorf("fake origin temporarily unavailable")
	}
	return nil
}

// UploadData implements uploaders.Uploader with a bounded retry loop,
// matching the production uploaders' behavior.
func (f *FakeUploader) UploadData(data []byte, dstPathFile string, headers map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < f.maxRetries(); attempt++ {
		f.callCount[dstPathFile]++

		if err := f.classifyFailure(); err != nil {
			lastErr = err
			// Permanent failures are not retried.
			if errors.Is(err, uploaders.ErrPermanentUploadError) {
				return err
			}
			if attempt < f.maxRetries()-1 {
				time.Sleep(time.Duration(attempt+1) * f.RetryDelay)
			}
			continue
		}

		// Only a fully successful attempt materializes the object.
		body := make([]byte, len(data))
		copy(body, data)
		f.Objects[dstPathFile] = body

		if strings.HasSuffix(filepath.Base(dstPathFile), ".m3u8") {
			f.record("manifest", dstPathFile, data)
		} else {
			f.record("upload", dstPathFile, nil)
		}
		return nil
	}

	return fmt.Errorf("%w: %s: %v", uploaders.ErrRetriesExhausted, dstPathFile, lastErr)
}

// UploadLocalFile implements uploaders.Uploader.
func (f *FakeUploader) UploadLocalFile(localFilename string, dstPathFile string, headers map[string]string) error {
	data, err := ioutil.ReadFile(localFilename)
	if err != nil {
		return err
	}
	return f.UploadData(data, dstPathFile, headers)
}

// UploadChunkedTransfer implements uploaders.ChunkedTransferUploader.
func (f *FakeUploader) UploadChunkedTransfer(dstPathFile string, headers map[string]string) (chan []byte, chan error) {
	writeChan := make(chan []byte)
	errChan := make(chan error, 1)

	go func() {
		var collected []byte
		for buf := range writeChan {
			collected = append(collected, buf...)
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		f.callCount[dstPathFile]++

		if f.ChunkedTransferError != nil {
			errChan <- fmt.Errorf("%w: %s: %v", uploaders.ErrChunkedTransfer, dstPathFile, f.ChunkedTransferError)
			return
		}

		body := make([]byte, len(collected))
		copy(body, collected)
		f.Objects[dstPathFile] = body
		f.record("upload", dstPathFile, nil)
		errChan <- nil
	}()

	return writeChan, errChan
}

// CallCount returns how many attempts (successful or not) targeted a path.
func (f *FakeUploader) CallCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount[path]
}

// HasObject reports whether the origin currently stores the path.
func (f *FakeUploader) HasObject(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.Objects[path]
	return ok
}

// GetObject returns the stored contents and whether the path exists.
func (f *FakeUploader) GetObject(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.Objects[path]
	return data, ok
}

// GetObjectByBase returns the object whose path ends with the given base
// name. It makes assertions independent of OS-specific path separators and
// relative/absolute path shapes.
func (f *FakeUploader) GetObjectByBase(base string) ([]byte, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for storedPath, data := range f.Objects {
		if filepath.Base(storedPath) == filepath.Base(base) {
			return data, storedPath, true
		}
	}
	return nil, "", false
}

// HasObjectByBase reports whether any stored object matches the base name.
func (f *FakeUploader) HasObjectByBase(base string) bool {
	_, _, ok := f.GetObjectByBase(base)
	return ok
}

// EventCount returns the number of recorded successful events.
func (f *FakeUploader) EventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Events)
}
