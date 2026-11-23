package httpuploader

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/sirupsen/logrus"
)

// Compile-time interface checks.
var (
	_ uploaders.Uploader                = (*HTTPUploader)(nil)
	_ uploaders.ChunkedTransferUploader = (*HTTPUploader)(nil)
)

// HTTPUploader HTTP uploader class class
type HTTPUploader struct {
	HTTPClient *http.Client

	Log                     *logrus.Logger
	HTTPSInsecure           bool
	HTTPScheme              string
	HTTPHost                string
	MaxHTTPRetries          int
	InitialHTTPRetryDelayMs int
}

// New Creates a chunk instance
func New(log *logrus.Logger, httpsInsecure bool, httpScheme string, httpHost string, maxHTTPRetries int, initialHTTPRetryDelayMs int) HTTPUploader {
	if log == nil {
		log = logrus.New()
		log.SetLevel(logrus.DebugLevel)
	}

	if maxHTTPRetries < 1 {
		maxHTTPRetries = 1
	}
	if initialHTTPRetryDelayMs < 0 {
		initialHTTPRetryDelayMs = 0
	}

	var tr = http.DefaultTransport
	if (strings.Compare(httpScheme, "https") == 0) && (httpsInsecure) {
		// Setup HTTPS client in dev env, skips CA verification
		log.Warn("Skipping CA cert verification!")
		tlsConfig := &tls.Config{InsecureSkipVerify: true}
		tr = &http.Transport{TLSClientConfig: tlsConfig}
	}
	client := http.Client{
		Transport: tr,
		Timeout:   0,
	}
	h := HTTPUploader{&client, log, httpsInsecure, httpScheme, httpHost, maxHTTPRetries, initialHTTPRetryDelayMs}

	return h
}

// UploadLocalFile Uploads a file from the filesystem.
//
// The file is re-opened on every attempt so the request body can be
// replayed after a retryable failure. The returned error is non-nil if
// every attempt failed (wrapping uploaders.ErrRetriesExhausted) or if the
// origin rejected the data permanently.
func (h *HTTPUploader) UploadLocalFile(localFilename string, dstPathFile string, headers map[string]string) error {
	return h.retryUpload(dstPathFile, func() error {
		f, errOpen := os.Open(localFilename)
		if errOpen != nil {
			h.Log.Error("ERROR reading ", localFilename, " (", dstPathFile, ")")
			return errOpen
		}
		defer f.Close()

		return h.uploadData(f, dstPathFile, headers)
	})
}

// UploadData Uploads data array.
//
// A fresh bytes.Reader is built for every attempt so retries resend the
// whole payload instead of an already-consumed (empty) body.
func (h *HTTPUploader) UploadData(data []byte, dstPathFile string, headers map[string]string) error {
	return h.retryUpload(dstPathFile, func() error {
		return h.uploadData(bytesReader(data), dstPathFile, headers)
	})
}

// UploadChunkedTransfer starts an HTTP chunked transfer upload.
//
// Chunked uploads cannot be retried: bytes are streamed to the origin as
// soon as they arrive, so once the connection breaks there is no way to
// replay them. Instead of swallowing the failure (the previous behavior)
// the terminal result is published on the returned error channel and can
// be observed when the caller closes the write channel.
//
// The returned write channel is the one the chunk writer feeds.
func (h *HTTPUploader) UploadChunkedTransfer(dstPathFile string, headers map[string]string) (chan []byte, chan error) {
	r, w := io.Pipe()
	writeChan := make(chan []byte)
	errChan := make(chan error, 1)

	// open request
	req := &http.Request{
		Method: "POST",
		URL: &url.URL{
			Scheme: h.HTTPScheme,
			Host:   h.HTTPHost,
			Path:   "/" + dstPathFile,
		},
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: -1,
		Body:          r,
		Header:        http.Header{},
	}

	// Add headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	go func() {
		defer w.Close()

		for buf := range writeChan {
			n, err := w.Write(buf)
			h.Log.Debug("Wrote ", n, " bytes to ", dstPathFile)
			if err != nil {
				// Closing the pipe with the error makes the request goroutine
				// below fail and report it on errChan.
				_ = w.CloseWithError(err)
				return
			}
			if n != len(buf) {
				shortWriteErr := io.ErrShortWrite
				_ = w.CloseWithError(shortWriteErr)
				return
			}
		}
	}()

	go func() {
		h.Log.Debug("Opening connection to upload to ", dstPathFile)
		h.Log.Debug("Req: ", req)
		resp, err := h.HTTPClient.Do(req)

		if err != nil {
			h.Log.Error("Error uploading to ", dstPathFile, ". Error: ", err)
			errChan <- fmt.Errorf("%w: %s: %v", uploaders.ErrChunkedTransfer, dstPathFile, err)
			return
		}
		defer resp.Body.Close()
		// Drain the body so the connection can be reused.
		_, _ = io.Copy(ioutil.Discard, resp.Body)

		if resp.StatusCode >= 400 {
			uploadErr := fmt.Errorf("%w: %s: HTTP status %d", uploaders.ErrChunkedTransfer, dstPathFile, resp.StatusCode)
			h.Log.Error(uploadErr)
			errChan <- uploadErr
			return
		}

		h.Log.Debug("Upload to ", dstPathFile, " complete")
		errChan <- nil
	}()

	return writeChan, errChan
}

// retryUpload runs attemptFn until it succeeds, returns a permanent error
// or the configured number of attempts is reached. The delay before
// attempt N grows linearly (N * InitialHTTPRetryDelayMs).
func (h *HTTPUploader) retryUpload(dstPathFile string, attemptFn func() error) error {
	var lastErr error

	for attempt := 0; attempt < h.MaxHTTPRetries; attempt++ {
		err := attemptFn()
		if err == nil {
			return nil
		}
		lastErr = err

		// Permanent failures must not be retried.
		if errors.Is(err, uploaders.ErrPermanentUploadError) {
			return err
		}

		h.Log.Warn("Retryable upload error for ", dstPathFile, " (attempt ", attempt+1, "/", h.MaxHTTPRetries, "): ", err)

		if attempt < h.MaxHTTPRetries-1 {
			// Linear backoff: intent * initial delay, matching the documented
			// "Value = intent * initialHTTPRetryDelay" flag semantics.
			time.Sleep(time.Duration((attempt+1)*h.InitialHTTPRetryDelayMs) * time.Millisecond)
		}
	}

	retErr := fmt.Errorf("%w: %s: %v", uploaders.ErrRetriesExhausted, dstPathFile, lastErr)
	h.Log.Error(retErr)
	return retErr
}

// uploadData performs a single HTTP POST. It classifies the response so
// the caller knows whether retrying makes sense.
func (h *HTTPUploader) uploadData(fileData io.Reader, dstPathFile string, headers map[string]string) error {
	req := &http.Request{
		Method: "POST",
		URL: &url.URL{
			Scheme: h.HTTPScheme,
			Host:   h.HTTPHost,
			Path:   "/" + dstPathFile,
		},
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: -1,
		Body:          ioutil.NopCloser(fileData),
		Header:        http.Header{},
	}

	// Add headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, errReq := h.HTTPClient.Do(req)
	if errReq != nil {
		// Transport-level failures (connection refused, timeout, ...) are
		// transient: the origin may come back.
		h.Log.Error("Error uploading to ", dstPathFile, ") Error: ", errReq)
		return errReq
	}
	defer resp.Body.Close()
	// The body must be fully consumed before the next attempt can reuse
	// this connection.
	_, _ = io.Copy(ioutil.Discard, resp.Body)

	if resp.StatusCode < 400 {
		// Done
		h.Log.Info("Upload to ", dstPathFile, " complete")
		return nil
	}

	if uploaders.IsRetryableHTTPStatus(resp.StatusCode) {
		// Transient origin response, the retry loop will try again.
		h.Log.Debug("Warning server busy, uploading to ", dstPathFile, ", RETRYING!")
		return fmt.Errorf("retryable upload error: HTTP status %d", resp.StatusCode)
	}

	// Not a retryable error (4xx other than 408/429): retrying cannot help.
	permErr := fmt.Errorf("%w: %s: HTTP status %d", uploaders.ErrPermanentUploadError, dstPathFile, resp.StatusCode)
	h.Log.Error(permErr)
	return permErr
}

// bytesReader returns a fresh reader over data on every call so each
// upload attempt resends the complete payload.
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}
