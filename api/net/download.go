package net

import (
	"context"
	"errors"
	"time"

	"github.com/alfg/openencoder/api/data"
	"github.com/alfg/openencoder/api/types"
)

// Download downloads a job source based on the driver setting.
//
// report, when not nil, is called periodically with the transfer percentage.
// The context aborts a running download when the job is cancelled; the transfer
// never writes to the database itself.
func Download(ctx context.Context, job types.Job, report ProgressFunc) error {
	db := data.New()
	setting, err := db.Settings.GetSetting(types.StorageDriver)
	if err != nil {
		return err
	}
	driver := setting.Value

	if driver == "s3" {
		return s3Download(ctx, job, report)
	} else if driver == "ftp" {
		return ftpDownload(ctx, job)
	}
	return errors.New("no driver set")
}

// GetPresignedURL gets a presigned URL from S3.
func GetPresignedURL(job types.Job) (string, error) {
	db := data.New()
	settings := db.Settings.GetSettings()

	config := S3Config{
		AccessKey:      types.GetSetting(types.S3AccessKey, settings),
		SecretKey:      types.GetSetting(types.S3SecretKey, settings),
		Provider:       types.GetSetting(types.S3Provider, settings),
		Region:         types.GetSetting(types.S3OutboundBucketRegion, settings),
		InboundBucket:  types.GetSetting(types.S3InboundBucket, settings),
		OutboundBucket: types.GetSetting(types.S3OutboundBucket, settings),
	}
	s3 := NewS3(config)
	str, err := s3.GetPresignedURL(job)
	if err != nil {
		return str, err
	}
	return str, nil
}

// S3Download sets the download function.
func s3Download(ctx context.Context, job types.Job, report ProgressFunc) error {
	db := data.New()
	settings := db.Settings.GetSettings()

	config := S3Config{
		AccessKey:      types.GetSetting(types.S3AccessKey, settings),
		SecretKey:      types.GetSetting(types.S3SecretKey, settings),
		Provider:       types.GetSetting(types.S3Provider, settings),
		Region:         types.GetSetting(types.S3OutboundBucketRegion, settings),
		InboundBucket:  types.GetSetting(types.S3InboundBucket, settings),
		OutboundBucket: types.GetSetting(types.S3OutboundBucket, settings),
	}
	s3 := NewS3(config)

	// Report transfer progress while the download runs. The reporter is
	// stopped and awaited before returning, so no progress write can happen
	// after the stage finished.
	stop := startProgressReporter(ctx, ProgressInterval, s3.ProgressPercent, report)
	err := s3.Download(ctx, job)
	stop()

	return err
}

// FTPDownload sets the FTP download function.
func ftpDownload(ctx context.Context, job types.Job) error {
	db := data.New()
	settings := db.Settings.GetSettings()

	addr := types.GetSetting(types.FTPAddr, settings)
	user := types.GetSetting(types.FTPUsername, settings)
	pass := types.GetSetting(types.FTPPassword, settings)

	f := NewFTP(addr, user, pass)
	return f.Download(ctx, job)
}

// startProgressReporter starts a goroutine that reports transfer progress every
// interval until the returned stop function is called or ctx is done. The
// returned stop function blocks until the reporter has exited so callers can
// guarantee that no progress write happens after it returns.
func startProgressReporter(ctx context.Context, interval time.Duration, read func() float64, report ProgressFunc) func() {
	if report == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = ProgressInterval
	}

	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				report(read())
			}
		}
	}()

	return func() {
		close(done)
		<-exited
	}
}
