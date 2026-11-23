package manifestgenerator

import (
	"bufio"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/hls"
	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/mediachunk"
	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/uploadertest"
)

// feedFixture streams the TS fixture through the generator, returning the
// first non-nil AddData error (or nil) and then always invoking Close.
func feedFixture(t *testing.T, mg *ManifestGenerator) (addErr error, closeErr error) {
	t.Helper()

	f, errOpen := os.Open("../fixture/testSmall.ts")
	if errOpen != nil {
		t.Fatalf("Error opening test file: %v", errOpen)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	buf := make([]byte, 0, 4*1024)
	for {
		n, err := reader.Read(buf[:cap(buf)])
		if n > 0 {
			if e := mg.AddData(buf[:n]); e != nil && addErr == nil {
				addErr = e
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			t.Fatalf("Error reading fixture: %v", err)
		}
	}

	closeErr = mg.Close()
	return addErr, closeErr
}

func newHTTPTestGenerator(t *testing.T, fake *uploadertest.FakeUploader, manifestType hls.ManifestTypes) (*ManifestGenerator, string) {
	t.Helper()
	dir, errTmp := ioutil.TempDir("", "mg-http-")
	if errTmp != nil {
		t.Fatalf("TempDir error: %v", errTmp)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	mg := New(nil,
		mediachunk.ChunkOutputModeHTTPRegular,
		hls.HlsOutputModeHTTP,
		dir,
		"chunk_",
		"chunklist.m3u8",
		4.0,
		ChunkInitStart,
		true,
		-1,
		-1,
		manifestType,
		3,
		0,
		nil,
		nil)

	// Inject the in-memory origin into both the chunk options resolution and
	// the HLS publisher.
	mg.options.injectUploader(fake)
	mg.hlsChunklist.SetUploaderForTest(fake)

	return &mg, dir
}

// manifestBaseNames returns the segment base names referenced by a manifest.
func manifestBaseNames(data []byte) []string {
	names := make([]string, 0)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, filepath.Base(line))
		}
	}
	return names
}

// TestGeneratorNormalUploadHappyPath: every chunk and every manifest reach
// the origin, the manifest references only existing chunks, and Close
// produces a final VOD manifest with ENDLIST.
func TestGeneratorNormalUploadHappyPath(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	mg, _ := newHTTPTestGenerator(t, fake, hls.Vod)

	addErr, closeErr := feedFixture(t, mg)
	if addErr != nil {
		t.Fatalf("AddData must not fail on a healthy origin: %v", addErr)
	}
	if closeErr != nil {
		t.Fatalf("Close must not fail on a healthy origin: %v", closeErr)
	}

	data, _, ok := fake.GetObjectByBase("chunklist.m3u8")
	if !ok {
		t.Fatal("Final manifest was not uploaded")
	}
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Errorf("VOD final manifest must contain ENDLIST:\n%s", string(data))
	}

	// Every referenced chunk must exist on the origin.
	for _, name := range manifestBaseNames(data) {
		if !fake.HasObjectByBase(name) {
			t.Errorf("Manifest references chunk %s that was never uploaded", name)
		}
	}

	// At least one chunk was produced.
	if len(manifestBaseNames(data)) == 0 {
		t.Error("No chunks referenced in the final manifest")
	}
}

// TestGeneratorRetryableFailureThenSuccess: a transient origin outage is
// retried and the stream completes successfully; ordering (chunk before
// manifest reference) holds for every published manifest event.
func TestGeneratorRetryableFailureThenSuccess(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	// The first several uploads fail transiently, then the origin recovers.
	fake.RetryableFailures = 4

	mg, _ := newHTTPTestGenerator(t, fake, hls.Vod)

	addErr, closeErr := feedFixture(t, mg)
	if addErr != nil {
		t.Fatalf("Stream should recover from transient failures, got: %v", addErr)
	}
	if closeErr != nil {
		t.Fatalf("Close should succeed after recovery, got: %v", closeErr)
	}

	// Validate every recorded manifest event only referenced chunks already
	// present at that moment (the core no-dangling-reference invariant).
	for _, ev := range fake.Events {
		if ev.Kind != "manifest" {
			continue
		}
		available := map[string]bool{}
		for _, obj := range ev.AvailableObjects {
			available[filepath.Base(obj)] = true
		}
		for _, uri := range ev.ReferencedURIs {
			if !available[filepath.Base(uri)] {
				t.Errorf("Manifest %s referenced %s before the chunk was available", ev.Path, uri)
			}
		}
	}
}

// TestGeneratorRetriesExhaustedSurfacesError: when the origin never
// recovers, AddData must surface a non-nil error (retries exhausted) and
// Close must also report failure rather than silently finishing.
func TestGeneratorRetriesExhaustedSurfacesError(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	fake.FailForeverRetryable = true

	mg, _ := newHTTPTestGenerator(t, fake, hls.Vod)

	addErr, closeErr := feedFixture(t, mg)

	if addErr == nil {
		t.Fatal("Expected AddData to fail once retries are exhausted")
	}
	if !errors.Is(addErr, uploaders.ErrRetriesExhausted) && !errors.Is(addErr, uploaders.ErrPermanentUploadError) {
		t.Errorf("Expected an upload exhaustion/permanent error, got: %v", addErr)
	}
	if closeErr == nil {
		t.Fatal("Close must report the terminal failure, not nil")
	}

	// A manifest that references a chunk may exist only if that chunk exists.
	for _, ev := range fake.Events {
		if ev.Kind != "manifest" {
			continue
		}
		available := map[string]bool{}
		for _, obj := range ev.AvailableObjects {
			available[filepath.Base(obj)] = true
		}
		for _, uri := range ev.ReferencedURIs {
			if !available[filepath.Base(uri)] {
				t.Errorf("Dangling reference: manifest %s -> %s with no chunk object", ev.Path, uri)
			}
		}
	}
}

// TestGeneratorCloseUploadsFinalManifest: on EOF the last chunk and the
// final manifest must run through the full upload flow, and no local temp
// files may remain after Close.
func TestGeneratorCloseUploadsFinalManifest(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	mg, _ := newHTTPTestGenerator(t, fake, hls.Vod)

	// Snapshot pre-existing OS temp files so the assertion only concerns
	// leftovers created by this test run.
	before, errBefore := filepath.Glob(filepath.Join(os.TempDir(), "*.tmp"))
	if errBefore != nil {
		t.Fatalf("glob temp dir: %v", errBefore)
	}
	beforeSet := map[string]bool{}
	for _, f := range before {
		beforeSet[f] = true
	}

	addErr, closeErr := feedFixture(t, mg)
	if addErr != nil || closeErr != nil {
		t.Fatalf("Healthy stream must finish cleanly, addErr=%v closeErr=%v", addErr, closeErr)
	}

	data, _, ok := fake.GetObjectByBase("chunklist.m3u8")
	if !ok {
		t.Fatal("Final manifest missing after Close")
	}
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Error("Final manifest must contain ENDLIST")
	}

	// The final chunk referenced must have been uploaded too.
	for _, name := range manifestBaseNames(data) {
		if !fake.HasObjectByBase(name) {
			t.Errorf("Final manifest references %s but the chunk object is missing", name)
		}
	}

	// Every temp file this run created must have been removed on Close.
	after, errAfter := filepath.Glob(filepath.Join(os.TempDir(), "*.tmp"))
	if errAfter != nil {
		t.Fatalf("glob temp dir: %v", errAfter)
	}
	for _, f := range after {
		if !beforeSet[f] {
			t.Errorf("Temp file left behind after Close: %s", f)
		}
	}
}

// TestGeneratorConcurrentManifestWritesNoDanglingReferences: drive several
// generators concurrently against one flaky origin and assert that no
// manifest ever references an unavailable chunk. This exercises the HLS
// mutex plus the chunk-before-manifest ordering under real concurrency.
func TestGeneratorConcurrentManifestWritesNoDanglingReferences(t *testing.T) {
	fake := uploadertest.NewFakeUploader()
	// A handful of transient failures spread across the concurrent streams.
	// With MaxRetries=10 no single object exhausts its budget.
	fake.RetryableFailures = 5

	const streams = 5
	errs := make(chan error, streams)

	for s := 0; s < streams; s++ {
		go func() {
			mg, _ := newHTTPTestGenerator(t, fake, hls.LiveWindow)
			addErr, closeErr := feedFixture(t, mg)
			if addErr != nil {
				errs <- addErr
				return
			}
			errs <- closeErr
		}()
	}

	for s := 0; s < streams; s++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent stream failed: %v", err)
		}
	}

	// Global invariant across every interleaving: a manifest event can only
	// reference chunks that were already available when it was recorded.
	for _, ev := range fake.Events {
		if ev.Kind != "manifest" {
			continue
		}
		available := map[string]bool{}
		for _, obj := range ev.AvailableObjects {
			available[filepath.Base(obj)] = true
		}
		for _, uri := range ev.ReferencedURIs {
			if !available[filepath.Base(uri)] {
				t.Errorf("Concurrent dangling reference in %s -> %s", ev.Path, uri)
			}
		}
	}
}
