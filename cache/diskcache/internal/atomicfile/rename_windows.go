//go:build windows

package atomicfile

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Concurrent writers of the same cache entry each commit by replacing the same
// destination. On Windows, MoveFileEx with MOVEFILE_REPLACE_EXISTING can
// transiently fail with "access denied" when another writer is in the middle of
// replacing that destination, so the commit is retried briefly.
const (
	renameRetries = 10
	renameBackoff = 10 * time.Millisecond
)

func atomicRenameRetry(oldpath, newpath string) error {
	var err error
	for i := 0; i < renameRetries; i++ {
		if err = atomicRename(oldpath, newpath); err == nil {
			return nil
		}
		if i < renameRetries-1 {
			time.Sleep(renameBackoff)
		}
	}
	return err
}

func atomicRename(oldpath, newpath string) error {
	src, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	dst, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	if err := windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
