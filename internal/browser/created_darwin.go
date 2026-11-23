package browser

import (
	"os"
	"syscall"
	"time"
)

func createdTime(_ string, info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Birthtimespec.Sec > 0 {
		return time.Unix(stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec)
	}
	return time.Time{}
}
