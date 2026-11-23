//go:build !windows

package atomicfile

import "os"

func atomicRename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// atomicRenameRetry commits the file. POSIX rename is atomic and replaces an
// existing destination without contention, so no retry is needed.
func atomicRenameRetry(oldpath, newpath string) error {
	return atomicRename(oldpath, newpath)
}
