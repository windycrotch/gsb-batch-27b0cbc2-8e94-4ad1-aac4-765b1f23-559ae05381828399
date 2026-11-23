//go:build !windows

package fileops

import "os"

// pinFileIdentity is a no-op on platforms where os.SameFile compares the
// device and inode captured by os.Lstat.
func pinFileIdentity(info os.FileInfo) {}
