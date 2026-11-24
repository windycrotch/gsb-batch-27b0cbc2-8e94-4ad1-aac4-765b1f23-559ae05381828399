package net

import (
	"context"
	"errors"

	"github.com/alfg/openencoder/api/data"
	"github.com/alfg/openencoder/api/types"
)

// Upload uploads a job based on the driver setting.
//
// report, when not nil, is called periodically with the upload percentage. The
// context aborts a running upload when the job is cancelled; the transfer never
// writes to the database itself.
func Upload(ctx context.Context, job types.Job, report ProgressFunc) error {
	db := data.New()
	driver, err := db.Settings.GetSetting(types.StorageDriver)
	if err != nil {
		return errors.New("no driver set")
	}

	if driver.Value == "s3" {
		return s3Upload(ctx, job, report)
	} else if driver.Value == "ftp" {
		return ftpUpload(ctx, job)
	}
	return errors.New("no driver set")
}

// s3Upload uploads a job's output to S3.
func s3Upload(ctx context.Context, job types.Job, report ProgressFunc) error {
	// Get credentials from settings.
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

	stop := startProgressReporter(ctx, ProgressInterval, s3.ProgressPercent, report)
	err := s3.Upload(ctx, job)
	stop()

	return err
}

// ftpUpload uploads a job's output to FTP.
func ftpUpload(ctx context.Context, job types.Job) error {
	db := data.New()
	settings := db.Settings.GetSettings()

	addr := types.GetSetting(types.FTPAddr, settings)
	user := types.GetSetting(types.FTPUsername, settings)
	pass := types.GetSetting(types.FTPPassword, settings)

	f := NewFTP(addr, user, pass)
	return f.Upload(ctx, job)
}
