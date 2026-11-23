package hls

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/uploadertest"
)

func newTestHls(t *testing.T, outputType OutputTypes, uploader uploaders.Uploader, chunklistName string) *Hls {
	t.Helper()
	h := New(nil, LiveWindow, 3, true, 4.0, 3, chunklistName, "", outputType, uploader)
	return &h
}

// TestSaveManifestToFileAtomicAndContent: normal local output must write a
// well-formed chunklist atomically and never leave a .tmp file behind.
func TestSaveManifestToFileAtomicAndContent(t *testing.T) {
	dir, errTmp := ioutil.TempDir("", "hls-file-")
	if errTmp != nil {
		t.Fatalf("TempDir error: %v", errTmp)
	}
	defer os.RemoveAll(dir)

	chunklist := filepath.Join(dir, "chunklist.m3u8")
	h := newTestHls(t, HlsOutputModeFile, nil, chunklist)

	if err := h.AddChunk(Chunk{FileName: filepath.Join(dir, "chunk_00000.ts"), DurationS: 4.0}, true); err != nil {
		t.Fatalf("AddChunk error: %v", err)
	}
	if err := h.AddChunk(Chunk{FileName: filepath.Join(dir, "chunk_00001.ts"), DurationS: 3.5}, true); err != nil {
		t.Fatalf("AddChunk error: %v", err)
	}

	data, errRead := ioutil.ReadFile(chunklist)
	if errRead != nil {
		t.Fatalf("Chunklist not written: %v", errRead)
	}
	content := string(data)
	if !strings.Contains(content, "chunk_00000.ts") || !strings.Contains(content, "chunk_00001.ts") {
		t.Errorf("Chunklist missing chunk references:\n%s", content)
	}
	if strings.Contains(content, "#EXT-X-ENDLIST") {
		t.Error("Live manifest must not contain ENDLIST before CloseManifest")
	}

	// No leftover temp files from the atomic write.
	entries, _ := ioutil.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".chunklist-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("Temp file left behind: %s", e.Name())
		}
	}

	if err := h.CloseManifest(true); err != nil {
		t.Fatalf("CloseManifest error: %v", err)
	}
	data, _ = ioutil.ReadFile(chunklist)
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Errorf("Final manifest must contain ENDLIST:\n%s", string(data))
	}
}

// TestManifestExternalRetryThenSuccessSelfHeals: when the first manifest
// publication fails but a later one succeeds, the successful write must
// contain the full up-to-date state (both chunks), restoring consistency
// at the destination instead of losing the update.
func TestManifestExternalRetryThenSuccessSelfHeals(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	// Bound the internal retries and make the first publication exhaust
	// them, so the failure is surfaced to the Hls layer and marks it dirty.
	fake.MaxRetries = 2
	fake.RetryableFailures = 3

	h := newTestHls(t, HlsOutputModeHTTP, fake, "out/chunklist.m3u8")

	// First AddChunk: the origin rejects every attempt, so the publication
	// fails and the manifest is marked dirty.
	err1 := h.AddChunk(Chunk{FileName: "out/chunk_00000.ts", DurationS: 4.0}, true)
	if err1 == nil {
		t.Fatal("Expected the first manifest publication to fail")
	}
	if !h.IsDirty() {
		t.Error("Manifest must be dirty after a failed publication")
	}

	// The failed publication must not have created the object.
	if fake.HasObject("out/chunklist.m3u8") {
		t.Error("Failed manifest upload must not create a partial object")
	}

	// The transient failures are now consumed; the second chunk +
	// publication succeeds and must republish the COMPLETE state, healing
	// the earlier failure.
	if err := h.AddChunk(Chunk{FileName: "out/chunk_00001.ts", DurationS: 4.0}, true); err != nil {
		t.Fatalf("Second publication must succeed, got: %v", err)
	}
	if h.IsDirty() {
		t.Error("Dirty flag must clear after a successful publication")
	}

	data, ok := fake.GetObject("out/chunklist.m3u8")
	if !ok {
		t.Fatal("Chunklist object missing after successful publication")
	}
	content := string(data)
	if !strings.Contains(content, "chunk_00000.ts") || !strings.Contains(content, "chunk_00001.ts") {
		t.Errorf("Healed manifest lost earlier chunk references:\n%s", content)
	}
}

// TestManifestRetriesExhaustedStaysDirtyAndErrors: when every publication
// keeps failing, AddChunk must keep returning an error and the manifest
// must remain dirty so callers know the destination is inconsistent.
func TestManifestRetriesExhaustedStaysDirtyAndErrors(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.FailForeverRetryable = true

	h := newTestHls(t, HlsOutputModeHTTP, fake, "out/chunklist.m3u8")

	err := h.AddChunk(Chunk{FileName: "out/chunk_00000.ts", DurationS: 4.0}, true)
	if err == nil {
		t.Fatal("Expected an error when the manifest cannot be published")
	}
	if !h.IsDirty() {
		t.Error("Manifest must remain dirty when publication keeps failing")
	}
	if fake.HasObject("out/chunklist.m3u8") {
		t.Error("No manifest object may exist after exhausted publications")
	}

	// Even CloseManifest must surface the failure instead of silently
	// producing a final manifest that never reached the origin.
	if err := h.CloseManifest(true); err == nil {
		t.Fatal("CloseManifest must return the final publication error")
	}
}

// TestCloseManifestRecoversAfterEarlierFailure: the explicit close path
// must go through the same publication flow and, once the origin is back,
// write a consistent final manifest containing ENDLIST.
func TestCloseManifestRecoversAfterEarlierFailure(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.FailForeverRetryable = true

	h := newTestHls(t, HlsOutputModeHTTP, fake, "out/chunklist.m3u8")
	if err := h.AddChunk(Chunk{FileName: "out/chunk_00000.ts", DurationS: 4.0}, true); err == nil {
		t.Fatal("Expected first publication to fail")
	}

	// Origin recovers.
	fake.FailForeverRetryable = false

	if err := h.CloseManifest(true); err != nil {
		t.Fatalf("CloseManifest must succeed once origin recovered: %v", err)
	}
	data, ok := fake.GetObject("out/chunklist.m3u8")
	if !ok {
		t.Fatal("Final manifest missing after recovery")
	}
	content := string(data)
	if !strings.Contains(content, "chunk_00000.ts") {
		t.Errorf("Recovered final manifest lost its chunk reference:\n%s", content)
	}
	if !strings.Contains(content, "#EXT-X-ENDLIST") {
		t.Errorf("Final manifest must contain ENDLIST:\n%s", content)
	}
}

// chunkURIs extracts the non-tag lines (segment URIs) from a manifest.
func chunkURIs(manifest string) []string {
	uris := make([]string, 0)
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
		}
	}
	return uris
}

// TestConcurrentAddChunkNeverReferencesUnavailableObject: hammer AddChunk
// from many goroutines against an origin that intermittently fails. Every
// manifest that reaches the origin may only reference chunks that were
// already available when it was published, and all internal state access
// must stay race-free (run this package with -race).
func TestConcurrentAddChunkNeverReferencesUnavailableObject(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.RetryableFailures = 25 // transient failures spread across goroutines

	const goroutines = 16

	// The manifest and the fake origin share a flat namespace in this test:
	// segments are stored and referenced by their base names so the
	// ordering assertion (referenced URI must already exist) is exact.
	for g := 0; g < goroutines; g++ {
		chunkPath := "chunk_g" + itoa(g) + ".ts"
		fake.Objects[chunkPath] = []byte("segment-data")
	}

	h := newTestHls(t, HlsOutputModeHTTP, fake, "out/chunklist.m3u8")

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			chunk := Chunk{
				FileName:  "chunk_g" + itoa(g) + ".ts",
				DurationS: 4.0,
			}
			// Retry internally until the publication is accepted, mirroring
			// how the generator treats a still-retryable manifest write.
			for attempt := 0; attempt < 50; attempt++ {
				if err := h.AddChunk(chunk, true); err == nil {
					return
				}
				// A failed publication must never have materialized a
				// manifest referencing an unavailable object.
				if data, exists := fake.GetObject("out/chunklist.m3u8"); exists {
					for _, uri := range chunkURIs(string(data)) {
						if !fake.HasObject(filepath.Base(uri)) {
							t.Errorf("Manifest references unavailable object %s", uri)
						}
					}
				}
				time.Sleep(time.Millisecond)
			}
			t.Errorf("goroutine %d could not publish its chunk", g)
		}(g)
	}
	wg.Wait()

	// Final invariant: the last published manifest references only objects
	// that exist on the fake origin.
	data, ok := fake.GetObject("out/chunklist.m3u8")
	if !ok {
		t.Fatal("No final manifest was published")
	}
	for _, uri := range chunkURIs(string(data)) {
		if !fake.HasObject(filepath.Base(uri)) {
			t.Errorf("Final manifest references unavailable object %s", uri)
		}
	}

	// And every successful manifest event honored the same ordering rule at
	// the moment it was recorded: the object snapshot taken during each
	// event must contain every URI that manifest referenced at that instant.
	// Base names are compared because HLS renders relative URIs (and on
	// Windows filepath.Rel yields "..\name" style prefixes).
	for _, ev := range fake.Events {
		if ev.Kind != "manifest" {
			continue
		}
		available := make(map[string]bool)
		for _, obj := range ev.AvailableObjects {
			available[filepath.Base(obj)] = true
		}
		for _, uri := range ev.ReferencedURIs {
			if !available[filepath.Base(uri)] {
				t.Errorf("Manifest event for %s referenced %s before it was available (available=%v)",
					ev.Path, uri, ev.AvailableObjects)
			}
		}
	}
}

// TestPermanentErrorIsReturned ensures a non-retryable origin error is
// propagated wrapped in the documented sentinel.
func TestPermanentErrorIsReturned(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.PermanentFailure = errors.New("access denied")

	h := newTestHls(t, HlsOutputModeHTTP, fake, "out/chunklist.m3u8")
	err := h.AddChunk(Chunk{FileName: "out/chunk_00000.ts", DurationS: 4.0}, true)
	if err == nil {
		t.Fatal("Expected a permanent error")
	}
	if !errors.Is(err, uploaders.ErrPermanentUploadError) {
		t.Errorf("Expected ErrPermanentUploadError, got: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
