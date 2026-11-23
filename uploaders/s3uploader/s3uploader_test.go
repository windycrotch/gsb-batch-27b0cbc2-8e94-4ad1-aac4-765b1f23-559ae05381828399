package s3uploader

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func getDateTimeStr() string {
	t := time.Now()
	return strconv.Itoa(t.Year()) + strconv.Itoa(int(t.Month())) + strconv.Itoa(t.Day()) + strconv.Itoa(t.Hour()) + strconv.Itoa(t.Minute()) + strconv.Itoa(t.Second())
}

// TestUploadLocalFile talks to a real S3 bucket using the machine's default
// credentials. It cannot run in CI / sandboxed environments (no credentials)
// and is skipped unless RUN_S3_INTEGRATION_TESTS=1 is set explicitly.
func TestUploadLocalFile(t *testing.T) {
	if os.Getenv("RUN_S3_INTEGRATION_TESTS") == "" {
		t.Skip("Skipping real S3 integration test; set RUN_S3_INTEGRATION_TESTS=1 to run it")
	}

	testFilePath := "../../fixture/testSmall.ts"
	headerName := "hName"
	headerValue := "hValue"
	UploadFilePath := "input/testID/testSmall-" + getDateTimeStr() + ".ts"

	// Used computer default creds
	awsCreds := AWSLocalCreds{Valid: false}
	// Upload to test bucket
	up := New(nil, "live-dist-test", "us-east-1", 10000, false, awsCreds)

	// Test metadata
	h := map[string]string{headerName: headerValue, "Content-Type": "video/MP2T"}

	ret := up.UploadLocalFile(testFilePath, UploadFilePath, h)
	if ret != nil {
		t.Error("Error uploading localfile. Err ", ret)
	}
}
