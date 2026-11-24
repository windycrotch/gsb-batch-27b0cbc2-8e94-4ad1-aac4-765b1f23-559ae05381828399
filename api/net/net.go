package net

import (
	"time"

	"github.com/alfg/openencoder/api/logging"
)

var log = logging.Log

// Settings that S3 uses.
const (
	EndpointAmazonAWS          = ".amazonaws.com"
	EndpointDigitalOceanSpaces = ".digitaloceanspaces.com"
	PresignedDuration          = 72 * time.Hour // 3 days.
	ProgressInterval           = time.Second * 5
)

// S3 Provider Endpoints with region.
var (
	EndpointDigitalOceanSpacesRegion = func(region string) string { return region + EndpointDigitalOceanSpaces }
	EndpointAmazonAWSRegion          = func(region string) string { return "s3." + region + EndpointAmazonAWS }
)

// ProgressFunc receives a transfer progress percentage in the range [0, 100].
type ProgressFunc func(progress float64)
