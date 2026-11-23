package fileops

import "golang.org/x/sys/windows"

// MoveFileEx without MOVEFILE_REPLACE_EXISTING enforces no-replace atomically,
// including dangling destination links.
func renameNoReplace(from, to string) error {
	fromp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	top, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromp, top, windows.MOVEFILE_WRITE_THROUGH)
}
