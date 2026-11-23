package s3uploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/sirupsen/logrus"
)

// Compile-time interface check.
var _ uploaders.Uploader = (*S3Uploader)(nil)

// Default retry policy applied when the uploader is created with New.
const (
	DefaultMaxRetries          = 3
	DefaultInitialRetryDelayMs = 100
)

// S3Uploader HTTP uploader class class
type S3Uploader struct {
	S3Session *s3.S3

	Log                        *logrus.Logger
	S3Bucket                   string
	S3Region                   string
	S3UploadTimeOutMs          int
	S3GrantReadToUploadedFiles bool
	AWSCreds                   AWSLocalCreds

	// MaxRetries is the maximum number of PutObject attempts for one object.
	MaxRetries int
	// InitialRetryDelayMs is the base of the linear backoff
	// (delay before attempt N = N * InitialRetryDelayMs).
	InitialRetryDelayMs int
}

// AWSLocalCreds local creds for debugging
type AWSLocalCreds struct {
	Valid     bool
	AWSId     string
	AWSSecret string
}

// New Creates a chunk instance
func New(log *logrus.Logger, s3Bucket string, s3Region string, s3UploadTimeOutMs int, s3GrantReadToUploadedFiles bool, awsCreds AWSLocalCreds) S3Uploader {
	if log == nil {
		log = logrus.New()
		log.SetLevel(logrus.DebugLevel)
	}

	// All clients require a Session. The Session provides the client with
	// shared configuration such as region, endpoint, and credentials. A
	// Session should be shared where possible to take advantage of
	// configuration and credential caching. See the session package for
	// more information.
	var s3Session *s3.S3 = nil
	if awsCreds.Valid {
		creds := credentials.NewStaticCredentials(awsCreds.AWSId, awsCreds.AWSSecret, "")
		_, err := creds.Get()
		if err != nil {
			log.Error("ERROR getting local credentials with ID ", awsCreds.AWSId)
		}
		awsConfig := aws.NewConfig().WithRegion(s3Region).WithCredentials(creds)
		awsSession := session.New()
		s3Session = s3.New(awsSession, awsConfig)
	} else {
		awsSession := session.Must(session.NewSessionWithOptions(session.Options{
			SharedConfigState:       session.SharedConfigEnable,
			AssumeRoleTokenProvider: stscreds.StdinTokenProvider,
		}))
		var awsConfig *aws.Config = nil
		if s3Region != "" {
			awsConfig = aws.NewConfig().WithRegion(s3Region)
		}
		s3Session = s3.New(awsSession, awsConfig)
	}
	return S3Uploader{
		S3Session:                  s3Session,
		Log:                        log,
		S3Bucket:                   s3Bucket,
		S3Region:                   s3Region,
		S3UploadTimeOutMs:          s3UploadTimeOutMs,
		S3GrantReadToUploadedFiles: s3GrantReadToUploadedFiles,
		AWSCreds:                   awsCreds,
		MaxRetries:                 DefaultMaxRetries,
		InitialRetryDelayMs:        DefaultInitialRetryDelayMs,
	}
}

// UploadLocalFile Uploads a file from the filesystem
func (s *S3Uploader) UploadLocalFile(localFilename string, dstPathFile string, headers map[string]string) error {
	f, errOpen := os.Open(localFilename)
	if errOpen != nil {
		s.Log.Error("ERROR reading  ", localFilename, "(", s.S3Bucket, "/", dstPathFile, ")")
		return errOpen
	}
	defer f.Close()

	fileInfo, _ := f.Stat()
	var size int64 = fileInfo.Size()
	buffer := make([]byte, size)
	if _, errRead := f.Read(buffer); errRead != nil && errRead != io.EOF {
		return errRead
	}

	return s.UploadData(buffer, dstPathFile, headers)
}

// UploadData upload bytes.
//
// Transient failures (origin 5xx / throttling / upload timeout / network
// errors) are retried up to MaxRetries times with linear backoff. A fresh
// timeout context is created for every attempt. If every attempt fails the
// returned error wraps uploaders.ErrRetriesExhausted so the caller knows
// the object is NOT available on S3 and must not be referenced yet.
func (s *S3Uploader) UploadData(buffer []byte, dstPathFile string, headers map[string]string) error {
	if s.Log == nil {
		s.Log = logrus.New()
	}
	if s.MaxRetries < 1 {
		s.MaxRetries = DefaultMaxRetries
	}
	var lastErr error

	for attempt := 0; attempt < s.MaxRetries; attempt++ {
		err := s.putObject(buffer, dstPathFile, headers)
		if err == nil {
			return nil
		}
		lastErr = err

		if errors.Is(err, uploaders.ErrPermanentUploadError) {
			return err
		}

		s.Log.Warn("Retryable S3 upload error for ", s.S3Bucket, "/", dstPathFile,
			" (attempt ", attempt+1, "/", s.MaxRetries, "): ", err)

		if attempt < s.MaxRetries-1 {
			time.Sleep(time.Duration((attempt+1)*s.InitialRetryDelayMs) * time.Millisecond)
		}
	}

	retErr := fmt.Errorf("%w: %s/%s: %v", uploaders.ErrRetriesExhausted, s.S3Bucket, dstPathFile, lastErr)
	s.Log.Error(retErr)
	return retErr
}

// putObject performs a single PutObject call with its own timeout context
// and classifies the resulting error as retryable or permanent.
func (s *S3Uploader) putObject(buffer []byte, dstPathFile string, headers map[string]string) error {
	// Create a context with a timeout that will abort the upload if it takes
	// more than the passed in timeout.
	ctx := context.Background()
	var cancelFn func()
	if s.S3UploadTimeOutMs > 0 {
		ctx, cancelFn = context.WithTimeout(ctx, time.Duration(s.S3UploadTimeOutMs)*time.Millisecond)
	}
	// Ensure the context is canceled to prevent leaking.
	// See context package for more information, https://golang.org/pkg/context/
	if cancelFn != nil {
		defer cancelFn()
	}

	s3Obj := s3.PutObjectInput{
		Bucket: aws.String(s.S3Bucket),
		Key:    aws.String(dstPathFile),
		Body:   bytes.NewReader(buffer),
	}

	// Add headers & contentType
	meta := map[string]*string{}
	for k, v := range headers {
		// Content type
		if strings.ToLower(k) == "content-type" {
			s3Obj.ContentType = aws.String(v)
		} else {
			meta[k] = aws.String(v)
		}
	}
	s3Obj.Metadata = meta

	if s.S3GrantReadToUploadedFiles {
		s3Obj.ACL = aws.String("public-read")
	}

	// Uploads the object to S3. The Context will interrupt the request if the
	// timeout expires.
	_, s3Err := s.S3Session.PutObjectWithContext(ctx, &s3Obj)
	if s3Err == nil {
		return nil
	}

	if !isRetryableS3Error(ctx, s3Err) {
		permErr := fmt.Errorf("%w: %s/%s: %v", uploaders.ErrPermanentUploadError, s.S3Bucket, dstPathFile, s3Err)
		s.Log.Error(permErr)
		return permErr
	}

	s.Log.Error("Retryable error uploading to ", s.S3Bucket, "/", dstPathFile, ". Err: ", s3Err)
	return s3Err
}

// isRetryableS3Error decides whether an S3 error can reasonably succeed on
// a new attempt: context deadlines (our upload timeout), transport-level
// AWS errors with a retryable HTTP status, and generic network errors.
func isRetryableS3Error(ctx context.Context, s3Err error) bool {
	if ctx.Err() == context.DeadlineExceeded {
		return true
	}

	var reqFailure awserr.RequestFailure
	if errors.As(s3Err, &reqFailure) {
		if uploaders.IsRetryableHTTPStatus(reqFailure.StatusCode()) {
			return true
		}
		return false
	}

	var awsErr awserr.Error
	if errors.As(s3Err, &awsErr) {
		switch awsErr.Code() {
		case request.CanceledErrorCode, request.ErrCodeResponseTimeout,
			request.ErrCodeRequestError, request.ErrCodeSerialization:
			return true
		}
	}

	if errors.Is(s3Err, context.DeadlineExceeded) || errors.Is(s3Err, context.Canceled) {
		return true
	}

	// Unknown / network errors default to retryable: the origin may be
	// temporarily unreachable and the data is still local.
	return true
}
