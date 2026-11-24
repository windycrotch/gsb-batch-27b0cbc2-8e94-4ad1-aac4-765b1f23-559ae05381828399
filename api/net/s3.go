package net

import (
	"context"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/alfg/openencoder/api/data"
	"github.com/alfg/openencoder/api/types"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// S3Config describes a configuration for setting up S3.
type S3Config struct {
	Provider       string
	Endpoint       string
	AccessKey      string
	SecretKey      string
	Region         string
	InboundBucket  string
	OutboundBucket string
}

// S3 creates a new S3 instance.
type S3 struct {
	Writer *ProgressWriter
	Reader *ProgressReader

	Config S3Config
}

// NewS3 creates a new S3 instance.
func NewS3(config S3Config) *S3 {
	config.Endpoint = getEndpoint(config.Provider, config.Region)

	return &S3{
		Config: config,
	}
}

// Download downloads source files from S3.
func (s *S3) Download(ctx context.Context, job types.Job) error {
	if ctx == nil {
		ctx = context.Background()
	}
	log.Info("downloading from S3: ", job.Source)

	// Open file for writing.
	file, err := os.Create(job.LocalSource)
	if err != nil {
		return err
	}
	defer file.Close()

	// Create session and client.
	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(s.Config.Endpoint),
		Region:      aws.String(s.Config.Region),
		Credentials: credentials.NewStaticCredentials(s.Config.AccessKey, s.Config.SecretKey, ""),
	})
	if err != nil {
		return err
	}
	s3Client := s3.New(sess)
	downloader := s3manager.NewDownloader(sess)

	parsedURL, _ := url.Parse(job.Source)
	key := parsedURL.Path

	size, err := getFileSize(s3Client, s.Config.InboundBucket, key)
	if err != nil {
		return err
	}
	log.Println("starting download, size: ", byteCountDecimal(size))

	// Get object input details.
	s.Writer = &ProgressWriter{writer: file, size: size, written: 0}
	objInput := s3.GetObjectInput{
		Bucket: aws.String(s.Config.InboundBucket),
		Key:    aws.String(key),
	}

	// Download file to local.
	if _, err = downloader.DownloadWithContext(ctx, s.Writer, &objInput); err != nil {
		log.Printf("download failed! deleting file: %s", file.Name())
		os.Remove(file.Name())
		return err
	}
	return ctx.Err()
}

// ProgressPercent returns the current transfer progress as a percentage between
// 0 and 100. Callers report it periodically; the transfer itself never writes to
// the database.
func (s *S3) ProgressPercent() float64 {
	if s.Writer != nil && s.Writer.size > 0 {
		written := atomic.LoadInt64(&s.Writer.written)
		return float64(written*100) / float64(s.Writer.size)
	}
	if s.Reader != nil && s.Reader.size > 0 {
		read := atomic.LoadInt64(&s.Reader.read)
		return float64(read*100) / float64(s.Reader.size)
	}
	return 0
}

// Upload uploads a file to S3.
func (s *S3) Upload(ctx context.Context, job types.Job) error {
	if ctx == nil {
		ctx = context.Background()
	}
	log.Info("uploading files to S3: ", job.Destination)
	defer log.Info("upload complete")

	// Get list of files in output dir.
	filelist, err := listFiles(path.Dir(job.LocalSource) + "/dst")
	if err != nil {
		return err
	}

	return s.uploadDir(ctx, filelist, job)
}

func (s *S3) uploadDir(ctx context.Context, filelist []string, job types.Job) error {
	for _, file := range filelist {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.uploadFile(ctx, file, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *S3) uploadFile(ctx context.Context, filePath string, job types.Job) error {
	log.Info("uploading file to S3.", job.Destination)

	// Open source path file.
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		log.Error("upload error: ", err)
		return err
	}

	// Set key.
	parsedURL, _ := url.Parse(job.Destination)
	key := parsedURL.Path + filepath.Base(filePath)

	s.Reader = &ProgressReader{
		fp:   file,
		size: fileInfo.Size(),
	}

	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(s.Config.Endpoint),
		Region:      aws.String(s.Config.Region),
		Credentials: credentials.NewStaticCredentials(s.Config.AccessKey, s.Config.SecretKey, ""),
	})
	if err != nil {
		return err
	}
	uploader := s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		u.PartSize = 5 * 1024 * 1024
		u.LeavePartsOnError = true
	})

	_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Body:   s.Reader,
		Bucket: aws.String(s.Config.OutboundBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

// S3ListFiles lists s3 objects for a given prefix.
func (s *S3) S3ListFiles(prefix string) (*s3.ListObjectsV2Output, error) {
	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(s.Config.Endpoint),
		Region:      aws.String(s.Config.Region),
		Credentials: credentials.NewStaticCredentials(s.Config.AccessKey, s.Config.SecretKey, ""),
	})
	if err != nil {
		return nil, err
	}
	svc := s3.New(sess)

	resp, err := svc.ListObjectsV2(
		&s3.ListObjectsV2Input{
			Bucket:    aws.String(s.Config.InboundBucket),
			Delimiter: aws.String("/"),
			Prefix:    aws.String(prefix),
		},
	)
	return resp, err
}

// GetPresignedURL generates a presigned URL from S3.
func (s *S3) GetPresignedURL(job types.Job) (string, error) {
	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(s.Config.Endpoint),
		Region:      aws.String(s.Config.Region),
		Credentials: credentials.NewStaticCredentials(s.Config.AccessKey, s.Config.SecretKey, ""),
	})
	if err != nil {
		return "", err
	}
	svc := s3.New(sess)

	parsedURL, _ := url.Parse(job.Source)
	key := parsedURL.Path

	objInput := s3.GetObjectInput{
		Bucket: aws.String(s.Config.InboundBucket),
		Key:    aws.String(key),
	}

	req, _ := svc.GetObjectRequest(&objInput)
	urlStr, err := req.Presign(PresignedDuration)

	return urlStr, err
}

// listFiles returns every regular file below dir. A missing directory is an
// error: it means the encode produced no output to upload.
func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

func getEndpoint(provider, region string) string {
	if strings.ToUpper(provider) == types.Custom {
		db := data.New()
		endpoint, err := db.Settings.GetSetting(types.S3Endpoint)
		if err != nil {
			return ""
		}
		return endpoint.Value
	} else if strings.ToUpper(provider) == types.DigitalOceanSpaces {
		return EndpointDigitalOceanSpacesRegion(region)
	}
	return EndpointAmazonAWSRegion(region)
}
