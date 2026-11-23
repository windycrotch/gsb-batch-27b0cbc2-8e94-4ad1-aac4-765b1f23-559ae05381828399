//go:build windows

package fileops

import "os"

// pinFileIdentity forces Windows to resolve the file's volume and index now.
// os.SameFile otherwise re-opens the path lazily at comparison time and cannot
// notice that the file at the path was replaced after Capture.
func pinFileIdentity(info os.FileInfo) { os.SameFile(info, info) }
