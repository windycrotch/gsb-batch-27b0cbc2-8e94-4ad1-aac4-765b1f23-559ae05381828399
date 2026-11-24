package helpers

import (
	"crypto/rand"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// CreateLocalSourcePath creates the working directories for a job GUID.
//
// Deprecated: use CreateRunSourcePath so every execution of a job gets its own
// temporary directory.
func CreateLocalSourcePath(workDir string, src string, ID string) string {
	// Get local destination path.
	tmpDir := workDir + "/" + ID + "/"
	os.MkdirAll(tmpDir, 0700)
	os.MkdirAll(tmpDir+"src", 0700)
	os.MkdirAll(tmpDir+"dst", 0700)
	return tmpDir + path.Base(src)
}

// GetTmpPath gets the working directory of a job GUID.
func GetTmpPath(workDir string, ID string) string {
	tmpDir := workDir + "/" + ID + "/"
	return tmpDir
}

// GetRunTmpPath gets the working directory of a single execution of a job.
//
// Every execution uses its own directory so that a restarted job can never
// remove or reuse the files of the execution it replaced.
func GetRunTmpPath(workDir string, guid string, runID string) string {
	return filepath.Join(workDir, safeSegment(guid), safeSegment(runID))
}

// CreateRunSourcePath creates the source and destination directories of a
// single execution of a job and returns the local source file path.
func CreateRunSourcePath(workDir string, src string, guid string, runID string) string {
	tmpDir := GetRunTmpPath(workDir, guid, runID)
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0700)
	os.MkdirAll(filepath.Join(tmpDir, "dst"), 0700)
	return filepath.Join(tmpDir, path.Base(src))
}

// RemoveRunTmpPath removes the working directory of a single execution.
func RemoveRunTmpPath(workDir string, guid string, runID string) error {
	return os.RemoveAll(GetRunTmpPath(workDir, guid, runID))
}

// RemoveStaleRunTmpPaths removes every working directory of a job except the
// one that belongs to runID. It is called when an execution starts so that
// leftovers of cancelled or crashed executions do not accumulate, while the
// directory of the current execution is left alone.
func RemoveStaleRunTmpPaths(workDir string, guid string, runID string) error {
	jobDir := filepath.Join(workDir, safeSegment(guid))
	entries, err := ioutil.ReadDir(jobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	keep := safeSegment(runID)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(jobDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// RemoveEmptyJobDir removes the per-job directory if it is empty. A non-empty
// directory (another execution is still using it) is left in place.
func RemoveEmptyJobDir(workDir string, guid string) {
	os.Remove(filepath.Join(workDir, safeSegment(guid)))
}

// safeSegment makes sure a path segment can not escape its parent directory.
func safeSegment(s string) string {
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// GenerateRandomKey creates a random key of the given length.
func GenerateRandomKey(length int) []byte {
	k := make([]byte, length)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil
	}
	return k
}
