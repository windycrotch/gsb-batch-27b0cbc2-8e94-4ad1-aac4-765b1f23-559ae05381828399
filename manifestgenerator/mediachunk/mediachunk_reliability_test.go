package mediachunk

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/uploadertest"
	"github.com/sirupsen/logrus"
)

func newExternalOptions(t *testing.T, outputType OutputTypes, uploader uploaders.Uploader) Options {
	t.Helper()
	dir, err := ioutil.TempDir("", "mediachunk-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	return Options{
		Log:                logrus.New(),
		OutputType:         outputType,
		EstimatedDurationS: 4.0,
		FileNumberLength:   5,
		GhostPrefix:        "",
		FileExtension:      ".ts",
		BasePath:           dir,
		ChunkBaseFilename:  "chunk_",
		Uploader:           uploader,
	}
}

// TestChunkCloseSuccessUploadsAndRemovesTemp: a healthy external upload
// makes the object available and deletes the local temp file.
func TestChunkCloseSuccessUploadsAndRemovesTemp(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	opts := newExternalOptions(t, ChunkOutputModeHTTPRegular, fake)

	chunk := New(0, opts)
	if err := chunk.InitializeChunk(); err != nil {
		t.Fatalf("InitializeChunk: %v", err)
	}
	if err := chunk.AddData([]byte("segment-bytes")); err != nil {
		t.Fatalf("AddData: %v", err)
	}

	tmp := chunk.tmpFilename
	if _, errStat := os.Stat(tmp); errStat != nil {
		t.Fatalf("temp file should exist before Close: %v", errStat)
	}

	if err := chunk.Close(4.0); err != nil {
		t.Fatalf("Close must succeed on a healthy origin: %v", err)
	}

	if !fake.HasObjectByBase(filepath.Base(chunk.GetFilename())) {
		t.Error("chunk object was not uploaded")
	}
	if _, errStat := os.Stat(tmp); !os.IsNotExist(errStat) {
		t.Errorf("temp file must be removed after Close, stat err=%v", errStat)
	}
}

// TestChunkCloseFailureReturnsErrorAndRemovesTemp: when retries are
// exhausted, Close returns the explicit error (no silent success) and the
// temp file is still cleaned up.
func TestChunkCloseFailureReturnsErrorAndRemovesTemp(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.FailForeverRetryable = true
	fake.MaxRetries = 2
	fake.RetryDelay = 0

	opts := newExternalOptions(t, ChunkOutputModeS3, fake)
	chunk := New(1, opts)
	if err := chunk.InitializeChunk(); err != nil {
		t.Fatalf("InitializeChunk: %v", err)
	}
	if err := chunk.AddData([]byte("segment-bytes")); err != nil {
		t.Fatalf("AddData: %v", err)
	}
	tmp := chunk.tmpFilename

	err := chunk.Close(4.0)
	if err == nil {
		t.Fatal("expected an error when uploads are exhausted")
	}
	if !errors.Is(err, uploaders.ErrRetriesExhausted) {
		t.Errorf("expected ErrRetriesExhausted, got: %v", err)
	}
	if _, errStat := os.Stat(tmp); !os.IsNotExist(errStat) {
		t.Errorf("temp file must still be removed after failure, stat err=%v", errStat)
	}
}

// TestChunkLocalFileCloseKeepsObject: local-file mode writes the object to
// disk and Close returns nil (no external uploader involved).
func TestChunkLocalFileCloseKeepsObject(t *testing.T) {
	opts := newExternalOptions(t, ChunkOutputModeFile, nil)
	chunk := New(2, opts)
	if err := chunk.InitializeChunk(); err != nil {
		t.Fatalf("InitializeChunk: %v", err)
	}
	if err := chunk.AddData([]byte("local-bytes")); err != nil {
		t.Fatalf("AddData: %v", err)
	}
	if err := chunk.Close(4.0); err != nil {
		t.Fatalf("local Close must not fail: %v", err)
	}
	data, errRead := ioutil.ReadFile(chunk.GetFilename())
	if errRead != nil {
		t.Fatalf("local chunk missing: %v", errRead)
	}
	if string(data) != "local-bytes" {
		t.Errorf("local chunk content mismatch: %q", string(data))
	}
}
