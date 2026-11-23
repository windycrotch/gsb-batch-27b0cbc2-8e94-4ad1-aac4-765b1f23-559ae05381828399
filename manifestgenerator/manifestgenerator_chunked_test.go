package manifestgenerator

import (
	"bufio"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/hls"
	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/mediachunk"
	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/uploadertest"
)

// TestGeneratorChunkedTransferFailureObservable: HTTP chunked transfer has
// no retry capability by design, but when the transfer breaks the failure
// must surface through Close instead of being silently swallowed, and the
// generator must then be in a poisoned (fatal error) state.
func TestGeneratorChunkedTransferFailureObservable(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	// Every chunked transfer reports an error when it completes.
	fake.ChunkedTransferError = errors.New("origin closed connection")

	dir, errTmp := os.MkdirTemp("", "mg-chunked-")
	if errTmp != nil {
		t.Fatalf("TempDir error: %v", errTmp)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	mg := New(nil,
		mediachunk.ChunkOutputModeHTTPChunkedTransfer,
		hls.HlsOutputModeHTTP,
		dir,
		"chunk_",
		"chunklist.m3u8",
		4.0,
		ChunkInitStart,
		true,
		-1,
		-1,
		hls.Vod,
		3,
		0,
		nil,
		nil)
	mg.options.injectUploader(fake)
	mg.hlsChunklist.SetUploaderForTest(fake)

	f, errOpen := os.Open("../fixture/testSmall.ts")
	if errOpen != nil {
		t.Fatalf("open fixture: %v", errOpen)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	buf := make([]byte, 0, 4*1024)
	var sawAddErr error
	for {
		n, err := reader.Read(buf[:cap(buf)])
		if n > 0 {
			if e := mg.AddData(buf[:n]); e != nil && sawAddErr == nil {
				sawAddErr = e
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			t.Fatalf("read fixture: %v", err)
		}
	}

	// Close finalizes the last chunk and waits for its transfer result.
	closeErr := mg.Close()

	if closeErr == nil && sawAddErr == nil {
		t.Fatal("Expected the chunked transfer failure to be observable, but no error was reported")
	}
	if closeErr != nil && !errors.Is(closeErr, uploaders.ErrChunkedTransfer) {
		t.Errorf("Expected ErrChunkedTransfer, got: %v", closeErr)
	}
	if sawAddErr != nil && !errors.Is(sawAddErr, uploaders.ErrChunkedTransfer) {
		t.Errorf("Expected AddData ErrChunkedTransfer, got: %v", sawAddErr)
	}
	if mg.FatalErr() == nil {
		t.Error("Generator must retain the terminal error in FatalErr")
	}
}
