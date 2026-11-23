//go:build !darwin && !linux

package browser

import (
	"os"
	"time"
)

func createdTime(_ string, _ os.FileInfo) time.Time { return time.Time{} }
