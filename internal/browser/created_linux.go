package browser

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func createdTime(path string, _ os.FileInfo) time.Time {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err == nil && stat.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec))
	}
	return time.Time{}
}
