// Package uploaders defines the common interface implemented by all the
// destinations that can receive media chunks and HLS manifests (local
// file, HTTP origin, S3).
//
// The contract is deliberately small so the manifest generator can remain
// agnostic of the concrete destination while still being able to:
//
//   - know for sure whether a chunk or a manifest upload succeeded,
//   - retry the failures that are worth retrying (503, timeouts, ...) a
//     bounded number of times with backoff,
//   - surface a terminal error to the caller once the retries are
//     exhausted instead of silently continuing and referencing objects
//     that do not exist on the origin.
package uploaders

import (
	"errors"
	"net/http"
)

// Sentinel errors returned by the concrete uploaders. Callers can use
// errors.Is to distinguish "the server keeps rejecting us, retries are
// exhausted" from a permanent rejection.
var (
	// ErrRetriesExhausted is returned when an upload kept failing with a
	// retryable error until the configured maximum number of attempts was
	// reached. The data for that object was not delivered.
	ErrRetriesExhausted = errors.New("upload failed: retries exhausted")

	// ErrPermanentUploadError wraps a failure that cannot be solved by
	// retrying (bad request, unauthorized, object not allowed, ...).
	ErrPermanentUploadError = errors.New("upload failed: permanent error")

	// ErrChunkedTransfer is returned when an HTTP chunked transfer upload
	// breaks. Chunked transfers cannot be retried (the bytes are streamed
	// as they arrive), but the failure must remain observable.
	ErrChunkedTransfer = errors.New("chunked transfer upload failed")
)

// Uploader is implemented by every destination able to store whole
// objects (HTTP regular uploads, S3, ...).
type Uploader interface {
	// UploadData uploads an in-memory object to dstPathFile.
	UploadData(data []byte, dstPathFile string, headers map[string]string) error

	// UploadLocalFile uploads the contents of a local file to
	// dstPathFile. The implementation is free to retry: the local file
	// stays available until the call returns.
	UploadLocalFile(localFilename string, dstPathFile string, headers map[string]string) error
}

// ChunkedTransferUploader is implemented by uploaders that can stream a
// growing object using HTTP chunked transfer encoding. There is no retry
// for this mode, but the error must be delivered through the channel
// returned alongside the write channel.
type ChunkedTransferUploader interface {
	// UploadChunkedTransfer starts a chunked upload and returns the
	// channel used to feed the object bytes plus a channel that will
	// receive at most one error (nil on success) once the transfer ends.
	UploadChunkedTransfer(dstPathFile string, headers map[string]string) (chan []byte, chan error)
}

// IsRetryableHTTPStatus reports whether an HTTP status code represents a
// transient condition worth retrying: request timeouts, rate limiting and
// any 5xx response from the origin.
func IsRetryableHTTPStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
