package s3uploader

import (
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/sirupsen/logrus"
)

// TestIsRetryableS3ErrorClassification covers the retry decision table
// without performing any network call.
func TestIsRetryableS3ErrorClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A context that already timed out => retryable.
	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer expiredCancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{
			name: "deadline exceeded on context",
			ctx:  expiredCtx,
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "AWS request failure 503",
			ctx:  ctx,
			err:  awserr.NewRequestFailure(awserr.New("ServiceUnavailable", "busy", nil), http.StatusServiceUnavailable, "req-1"),
			want: true,
		},
		{
			name: "AWS request failure 500",
			ctx:  ctx,
			err:  awserr.NewRequestFailure(awserr.New("InternalError", "boom", nil), http.StatusInternalServerError, "req-2"),
			want: true,
		},
		{
			name: "AWS request failure 403 is permanent",
			ctx:  ctx,
			err:  awserr.NewRequestFailure(awserr.New("Forbidden", "no", nil), http.StatusForbidden, "req-3"),
			want: false,
		},
		{
			name: "AWS request failure 400 is permanent",
			ctx:  ctx,
			err:  awserr.NewRequestFailure(awserr.New("BadRequest", "bad", nil), http.StatusBadRequest, "req-4"),
			want: false,
		},
		{
			name: "generic network error defaults to retryable",
			ctx:  ctx,
			err:  errors.New("dial tcp: connection reset"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableS3Error(tc.ctx, tc.err); got != tc.want {
				t.Errorf("isRetryableS3Error() = %v, want %v", got, tc.want)
			}
		})
	}
}

// newS3AgainstTestServer builds an S3Uploader whose SDK client points at
// the provided test server, using dummy static credentials (no real AWS
// access happens).
func newS3AgainstTestServer(t *testing.T, server *httptest.Server, maxRetries int) S3Uploader {
	t.Helper()
	u, errURL := url.Parse(server.URL)
	if errURL != nil {
		t.Fatalf("parse server url: %v", errURL)
	}

	creds := credentials.NewStaticCredentials("test-key", "test-secret", "")
	awsCfg := aws.NewConfig().
		WithRegion("us-east-1").
		WithCredentials(creds).
		WithEndpoint(server.URL).
		WithS3ForcePathStyle(true).
		WithDisableSSL(true).
		// The uploader owns the retry policy; disable the SDK's internal
		// retries so one PutObjectWithContext call equals one HTTP attempt.
		WithMaxRetries(0)

	sess, errSession := session.NewSession(awsCfg)
	if errSession != nil {
		t.Fatalf("new session: %v", errSession)
	}
	s3Client := s3.New(sess)
	_ = u

	return S3Uploader{
		S3Session:           s3Client,
		Log:                 logrus.New(),
		S3Bucket:            "test-bucket",
		S3Region:            "us-east-1",
		S3UploadTimeOutMs:   5000,
		MaxRetries:          maxRetries,
		InitialRetryDelayMs: 1,
	}
}

// s3Response writes a minimal XML response the S3 SDK can parse.
func s3Response(rw http.ResponseWriter, status int) {
	rw.Header().Set("Content-Type", "application/xml")
	rw.WriteHeader(status)
	_, _ = rw.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>Err</Code><Message>test</Message></Error>`))
}

// TestS3UploadRetriesThenSuccess: a 503 followed by a 200 must be retried
// with backoff and eventually succeed, with bounded attempt count.
func TestS3UploadRetriesThenSuccess(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = ioutil.ReadAll(req.Body)
		if atomic.AddInt32(&attempts, 1) <= 2 {
			s3Response(rw, http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/xml")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`<PutObjectOutput><ETag>"etag"</ETag></PutObjectOutput>`))
	}))
	defer server.Close()

	up := newS3AgainstTestServer(t, server, 5)
	if err := up.UploadData([]byte("s3-data"), "key/retry.ts", nil); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// TestS3UploadRetriesExhausted: persistent 503s must yield an
// ErrRetriesExhausted after exactly MaxRetries attempts.
func TestS3UploadRetriesExhausted(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = ioutil.ReadAll(req.Body)
		atomic.AddInt32(&attempts, 1)
		s3Response(rw, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	const maxRetries = 3
	up := newS3AgainstTestServer(t, server, maxRetries)
	err := up.UploadData([]byte("s3-data"), "key/fail.ts", nil)
	if err == nil {
		t.Fatal("expected an error after retries exhausted")
	}
	if !errors.Is(err, uploaders.ErrRetriesExhausted) {
		t.Errorf("expected ErrRetriesExhausted, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != maxRetries {
		t.Errorf("expected %d attempts, got %d", maxRetries, got)
	}
}

// TestS3UploadPermanentErrorNotRetried: a 403 must fail immediately.
func TestS3UploadPermanentErrorNotRetried(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = ioutil.ReadAll(req.Body)
		atomic.AddInt32(&attempts, 1)
		s3Response(rw, http.StatusForbidden)
	}))
	defer server.Close()

	up := newS3AgainstTestServer(t, server, 5)
	err := up.UploadData([]byte("s3-data"), "key/forbidden.ts", nil)
	if err == nil {
		t.Fatal("expected a permanent error")
	}
	if !errors.Is(err, uploaders.ErrPermanentUploadError) {
		t.Errorf("expected ErrPermanentUploadError, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("permanent error must not be retried, expected 1 attempt, got %d", got)
	}
}
