package mjpeg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// -----------------------------------------------------------------------------
// AVI parsing helpers used to check the structural consistency of the output.
// -----------------------------------------------------------------------------

type chunkInfo struct {
	id      string
	pos     int64 // position of the chunk header (the 4-byte id)
	payload int64 // position of the chunk payload
	size    int32
}

func (c chunkInfo) end() int64 { return c.payload + int64(c.size) }

type idxEntry struct {
	id     string
	offset int32
	length int32
}

type parsedAVI struct {
	data []byte

	totalFrames int32 // dwTotalFrames in 'avih'
	strhLength  int32 // dwLength in 'strh'

	movi       chunkInfo
	moviData   int64 // position right after the 'movi' list type; index offsets are relative to this
	frameChunk []chunkInfo
	idx1       chunkInfo
	idxEntries []idxEntry
}

func le32(b []byte) int32  { return int32(binary.LittleEndian.Uint32(b)) }
func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }

func parseChunks(t *testing.T, p *parsedAVI, start, end int64) []chunkInfo {
	t.Helper()
	var chunks []chunkInfo
	for pos := start; pos+8 <= end; {
		c := chunkInfo{
			id:      string(p.data[pos : pos+4]),
			pos:     pos,
			payload: pos + 8,
			size:    le32(p.data[pos+4 : pos+8]),
		}
		if c.end() > end {
			t.Fatalf("chunk %q at %d: payload end %d exceeds enclosing end %d", c.id, pos, c.end(), end)
		}
		chunks = append(chunks, c)
		pos = c.end()
		if pos&1 == 1 {
			pos++
		}
	}
	return chunks
}

func parseAVI(t *testing.T, data []byte) *parsedAVI {
	t.Helper()
	if len(data) < 12 {
		t.Fatalf("file too short: %d bytes", len(data))
	}
	if got := string(data[:4]); got != "RIFF" {
		t.Fatalf("RIFF signature = %q", got)
	}
	riffSize := le32(data[4:8])
	if int64(riffSize) != int64(len(data))-8 {
		t.Fatalf("RIFF size field = %d, want %d (file size - 8)", riffSize, len(data)-8)
	}
	if got := string(data[8:12]); got != "AVI " {
		t.Fatalf("AVI signature = %q", got)
	}

	p := &parsedAVI{data: data}
	var hdrl, movi *chunkInfo
	for _, c := range parseChunks(t, p, 12, int64(len(data))) {
		if c.id != "LIST" {
			continue
		}
		typ := string(data[c.payload : c.payload+4])
		cc := c
		switch typ {
		case "hdrl":
			if hdrl == nil {
				hdrl = &cc
			}
		case "movi":
			movi = &cc
		}
	}
	if hdrl == nil {
		t.Fatal("no 'hdrl' LIST chunk found")
	}
	if movi == nil {
		t.Fatal("no 'movi' LIST chunk found")
	}
	p.movi = *movi
	// Index offsets are measured from the 'movi' fourcc (the list type), which
	// is where the list payload begins.
	p.moviData = movi.payload

	// avih / strh
	for _, c := range parseChunks(t, p, hdrl.payload+4, hdrl.end()) {
		switch c.id {
		case "avih":
			if c.size < 20 {
				t.Fatalf("avih chunk too small: %d", c.size)
			}
			p.totalFrames = le32(data[c.payload+16 : c.payload+20])
		case "LIST":
			if string(data[c.payload:c.payload+4]) != "strl" {
				continue
			}
			for _, sc := range parseChunks(t, p, c.payload+4, c.end()) {
				if sc.id == "strh" {
					if sc.size < 36 {
						t.Fatalf("strh chunk too small: %d", sc.size)
					}
					p.strhLength = le32(data[sc.payload+32 : sc.payload+36])
				}
			}
		}
	}

	// Frames inside 'movi'.
	p.frameChunk = parseChunks(t, p, movi.payload+4, movi.end())

	// idx1 comes after 'movi'.
	for _, c := range parseChunks(t, p, movi.end(), int64(len(data))) {
		if c.id == "idx1" {
			p.idx1 = c
			break
		}
	}
	if p.idx1.size != 0 {
		for pos := p.idx1.payload; pos < p.idx1.end(); pos += 16 {
			p.idxEntries = append(p.idxEntries, idxEntry{
				id:     string(data[pos : pos+4]),
				offset: le32(data[pos+8 : pos+12]),
				length: le32(data[pos+12 : pos+16]),
			})
		}
	}

	return p
}

// verifyConsistent checks that all structural invariants hold: the frame count
// fields match the actual frames, and the index entries point at the actual
// frame chunks with the right lengths.
func (p *parsedAVI) verifyConsistent(t *testing.T) {
	t.Helper()
	n := len(p.frameChunk)
	if int(p.totalFrames) != n {
		t.Errorf("avih frame count = %d, want %d", p.totalFrames, n)
	}
	if int(p.strhLength) != n {
		t.Errorf("strh frame count = %d, want %d", p.strhLength, n)
	}
	if len(p.idxEntries) != n {
		t.Errorf("idx1 has %d entries, want %d", len(p.idxEntries), n)
	}
	if want := int32(16 * n); p.idx1.size != want {
		t.Errorf("idx1 chunk size = %d, want %d", p.idx1.size, want)
	}
	// The index entries must describe exactly the frame chunks in order.
	for i := 0; i < n && i < len(p.idxEntries); i++ {
		c, e := p.frameChunk[i], p.idxEntries[i]
		if e.id != "00dc" || c.id != "00dc" {
			t.Errorf("frame %d: ids = %q / %q, want 00dc/00dc", i, c.id, e.id)
		}
		if wantOff := int32(c.pos - p.moviData); e.offset != wantOff {
			t.Errorf("frame %d: index offset = %d, want %d", i, e.offset, wantOff)
		}
		if wantLen := c.size; e.length != wantLen {
			t.Errorf("frame %d: index length = %d, want %d", i, e.length, wantLen)
		}
	}
	// The movi list size must cover exactly the frame chunks. RIFF padding of a
	// trailing odd-sized chunk is not counted in the chunk size, so the idx1
	// chunk may start one byte (the pad byte) after the movi list.
	moviEnd := p.movi.end()
	if p.idx1.pos != moviEnd && !(moviEnd&1 == 1 && p.idx1.pos == moviEnd+1) {
		t.Errorf("movi list ends at %d, idx1 starts at %d", moviEnd, p.idx1.pos)
	}
}

func (p *parsedAVI) framePayloads() [][]byte {
	out := make([][]byte, len(p.frameChunk))
	for i, c := range p.frameChunk {
		out[i] = p.data[c.payload:c.end()]
	}
	return out
}

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

func mustNew(t *testing.T, dir string) (*aviWriter, string) {
	t.Helper()
	path := filepath.Join(dir, "test.avi")
	aw, err := New(path, 32, 24, 5)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return aw.(*aviWriter), path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (stat err: %v), want it removed", path, err)
	}
}

// frameData returns a fake JPEG frame with the given tag written into it.
// The tag is stored at index 2 (after the SOI marker) so it can be recovered
// from the written file.
func frameData(tag int, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(tag)
	}
	b[0] = 0xFF
	b[1] = 0xD8
	b[2] = byte(tag)
	b[size-2] = 0xFF
	b[size-1] = 0xD9
	return b
}

// frameTag extracts the tag stored by frameData.
func frameTag(payload []byte) int { return int(payload[2]) }

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestWriteAndClose verifies the normal happy path: the file is structurally
// consistent, the temporary index file is removed, and repeated Close calls
// return the result of the first one without panicking.
func TestWriteAndClose(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	const n = 7
	for i := 0; i < n; i++ {
		if err := aw.AddFrame(frameData(i+1, 100+i*13)); err != nil {
			t.Fatalf("AddFrame(%d) failed: %v", i, err)
		}
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	assertNotExist(t, path+".idx_")
	assertNotExist(t, aw.idxFile)

	for i := 0; i < 3; i++ {
		if err := aw.Close(); err != nil {
			t.Fatalf("repeated Close() #%d failed: %v", i+2, err)
		}
	}
	assertNotExist(t, path+".idx_")

	p := parseAVI(t, readFile(t, path))
	if len(p.frameChunk) != n {
		t.Fatalf("wrote %d frames, want %d", len(p.frameChunk), n)
	}
	p.verifyConsistent(t)

	for i, payload := range p.framePayloads() {
		if !bytes.Equal(payload, frameData(i+1, 100+i*13)) {
			t.Errorf("frame %d payload mismatch (len %d)", i, len(payload))
		}
	}
}

// TestAddFrameAfterCloseRejected verifies frames submitted after Close are
// rejected and do not modify the finalized file.
func TestAddFrameAfterCloseRejected(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	if err := aw.AddFrame(frameData(1, 64)); err != nil {
		t.Fatalf("AddFrame failed: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	before := readFile(t, path)

	if err := aw.AddFrame(frameData(2, 64)); !errors.Is(err, ErrClosed) {
		t.Fatalf("AddFrame after Close = %v, want ErrClosed", err)
	}
	if after := readFile(t, path); !bytes.Equal(before, after) {
		t.Error("AddFrame after Close modified the file")
	}
	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)
	if len(p.frameChunk) != 1 {
		t.Errorf("frame count = %d, want 1", len(p.frameChunk))
	}
}

// TestConcurrentAddFrames submits frames from many goroutines. Frame order in
// the file must match the order of successful AddFrame returns, and the index
// must describe exactly the written frames.
func TestConcurrentAddFrames(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	const (
		goroutines = 8
		perRoutine = 25
	)
	var (
		recMu sync.Mutex // held across AddFrame so recorded order is return order
		order []int
		wg    sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perRoutine; i++ {
				tag := g*perRoutine + i
				data := frameData(tag+1, 200+tag%37)
				recMu.Lock()
				err := aw.AddFrame(data)
				if err == nil {
					order = append(order, tag)
				}
				recMu.Unlock()
				if err != nil {
					t.Errorf("AddFrame(tag %d) failed: %v", tag, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	assertNotExist(t, path+".idx_")

	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)
	if len(p.frameChunk) != goroutines*perRoutine {
		t.Fatalf("frame count = %d, want %d", len(p.frameChunk), goroutines*perRoutine)
	}
	for i, payload := range p.framePayloads() {
		tag := order[i]
		if want := frameData(tag+1, 200+tag%37); !bytes.Equal(payload, want) {
			t.Fatalf("frame %d in file has tag %d, want %d", i, frameTag(payload), tag+1)
		}
	}
}

// TestConcurrentAddFramesUnordered submits frames concurrently without any
// external serialization: every successful frame must be present exactly once
// and the resulting file must be structurally consistent.
func TestConcurrentAddFramesUnordered(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	const total = 120
	var (
		mu      sync.Mutex
		success []int
		wg      sync.WaitGroup
	)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(tag int) {
			defer wg.Done()
			err := aw.AddFrame(frameData(tag+1, 300+tag%41))
			if err != nil {
				t.Errorf("AddFrame(%d) failed: %v", tag, err)
				return
			}
			mu.Lock()
			success = append(success, tag)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)

	if len(success) != total || len(p.frameChunk) != total {
		t.Fatalf("successful frames = %d, frames in file = %d, want %d", len(success), len(p.frameChunk), total)
	}
	seen := make(map[byte]bool, total)
	for _, payload := range p.framePayloads() {
		tag := frameTag(payload)
		if seen[byte(tag)] {
			t.Fatalf("frame tag %d appears more than once", tag)
		}
		seen[byte(tag)] = true
	}
}

// TestConcurrentCloseAndAddFrame interleaves Close with AddFrame calls: this
// must not panic, every AddFrame must return either nil or ErrClosed, and all
// frames which succeeded must be present in the finished file.
func TestConcurrentCloseAndAddFrame(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		aw, path := mustNew(t, dir)

		const goroutines = 8
		var (
			mu        sync.Mutex
			succeeded int
			wg        sync.WaitGroup
		)
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					err := aw.AddFrame(frameData(g*10+i+1, 128))
					if err != nil {
						if !errors.Is(err, ErrClosed) {
							t.Errorf("AddFrame returned unexpected error: %v", err)
						}
						continue
					}
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}(g)
		}
		// Concurrent closers.
		var closers []error
		for c := 0; c < 4; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := aw.Close()
				mu.Lock()
				closers = append(closers, err)
				mu.Unlock()
			}()
		}
		wg.Wait()

		for i, err := range closers {
			if err != nil {
				t.Fatalf("round %d: Close() #%d failed: %v", round, i, err)
			}
		}
		assertNotExist(t, path+".idx_")

		p := parseAVI(t, readFile(t, path))
		p.verifyConsistent(t)
		if len(p.frameChunk) != succeeded {
			t.Fatalf("round %d: successful AddFrames = %d, frames in file = %d", round, succeeded, len(p.frameChunk))
		}
	}
}

// TestFrameFailureIsSticky verifies that after a frame write fails, all
// further AddFrame calls fail and leave the file untouched, while the index of
// the already written frames stays correct.
func TestFrameFailureIsSticky(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	good1 := frameData(1, 81)
	good2 := frameData(2, 91)
	bad := frameData(3, 101)

	if err := aw.AddFrame(good1); err != nil {
		t.Fatalf("AddFrame(good1) failed: %v", err)
	}
	if err := aw.AddFrame(good2); err != nil {
		t.Fatalf("AddFrame(good2) failed: %v", err)
	}

	// Inject a failure exactly when the bad frame data is written.
	aw.writeHook = func(p []byte) (int, error) {
		if bytes.Contains(p, bad) {
			return 0, errors.New("simulated disk failure")
		}
		return aw.avif.Write(p)
	}
	err := aw.AddFrame(bad)
	if err == nil {
		t.Fatal("AddFrame(bad) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "simulated disk failure") {
		t.Errorf("error does not expose the cause: %v", err)
	}

	afterFailure := readFile(t, path)
	idxAfterFailure := readFile(t, path+".idx_")

	// Further frames must be rejected and must not touch the files.
	aw.writeHook = func(p []byte) (int, error) {
		t.Errorf("file written after a frame failure")
		return aw.avif.Write(p)
	}
	for i := 0; i < 3; i++ {
		if err := aw.AddFrame(frameData(10+i, 64)); err == nil {
			t.Fatalf("AddFrame after failure #%d succeeded, want an error", i+1)
		}
	}
	if got := readFile(t, path); !bytes.Equal(afterFailure, got) {
		t.Error("avi file changed after frame failure")
	}
	if got := readFile(t, path+".idx_"); !bytes.Equal(idxAfterFailure, got) {
		t.Error("index file changed after frame failure")
	}
	aw.writeHook = nil

	// Close reports the failure but must still clean up the index file.
	if err := aw.Close(); err == nil {
		t.Error("Close() after a frame failure returned nil, want an error")
	}
	assertNotExist(t, path+".idx_")

	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)
	if len(p.frameChunk) != 2 {
		t.Fatalf("frame count = %d, want 2", len(p.frameChunk))
	}
	if !bytes.Equal(p.framePayloads()[0], good1) || !bytes.Equal(p.framePayloads()[1], good2) {
		t.Error("successfully written frames were corrupted")
	}
}

// TestCloseIndexWriteFailure verifies that a failure while splicing the index
// into the avi file is reported and leaves no temporary index file behind.
func TestCloseIndexWriteFailure(t *testing.T) {
	t.Run("idx1 header write", func(t *testing.T) {
		dir := t.TempDir()
		aw, path := mustNew(t, dir)
		for i := 0; i < 5; i++ {
			if err := aw.AddFrame(frameData(i+1, 100)); err != nil {
				t.Fatalf("AddFrame failed: %v", err)
			}
		}
		calls := 0
		aw.writeHook = func(p []byte) (int, error) {
			calls++
			if calls == 1 { // writing the "idx1" signature
				return 0, errors.New("simulated write failure")
			}
			return aw.avif.Write(p)
		}
		if err := aw.Close(); err == nil {
			t.Fatal("Close() succeeded, want an error")
		}
		assertNotExist(t, path+".idx_")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("avi file missing after failed Close: %v", err)
		}
	})

	t.Run("index data copy", func(t *testing.T) {
		dir := t.TempDir()
		aw, path := mustNew(t, dir)
		for i := 0; i < 5; i++ {
			if err := aw.AddFrame(frameData(i+1, 100)); err != nil {
				t.Fatalf("AddFrame failed: %v", err)
			}
		}
		calls := 0
		aw.writeHook = func(p []byte) (int, error) {
			calls++
			if calls >= 3 { // "idx1", its length, then the copied index data
				return 0, errors.New("simulated write failure")
			}
			return aw.avif.Write(p)
		}
		err := aw.Close()
		if err == nil {
			t.Fatal("Close() succeeded, want an error")
		}
		assertNotExist(t, path+".idx_")

		// Repeated Close must return the same error, not panic.
		if err2 := aw.Close(); err2 == nil || err2.Error() != err.Error() {
			t.Errorf("repeated Close() = %v, want %v", err2, err)
		}
	})

	t.Run("frame count backfill", func(t *testing.T) {
		dir := t.TempDir()
		aw, path := mustNew(t, dir)
		for i := 0; i < 3; i++ {
			if err := aw.AddFrame(frameData(i+1, 100)); err != nil {
				t.Fatalf("AddFrame failed: %v", err)
			}
		}
		calls := 0
		aw.writeHook = func(p []byte) (int, error) {
			calls++
			if calls >= 4 { // after idx1 signature, its length and the index data
				return 0, errors.New("simulated write failure")
			}
			return aw.avif.Write(p)
		}
		if err := aw.Close(); err == nil {
			t.Fatal("Close() succeeded, want an error")
		}
		assertNotExist(t, path+".idx_")
	})
}

// TestCloseIndexReadFailure covers a failing read of the temporary index.
func TestCloseIndexReadFailure(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)
	for i := 0; i < 3; i++ {
		if err := aw.AddFrame(frameData(i+1, 100)); err != nil {
			t.Fatalf("AddFrame failed: %v", err)
		}
	}
	// Close the index file behind the writer's back: the index copy will fail.
	if err := aw.idxf.Close(); err != nil {
		t.Fatalf("closing index file: %v", err)
	}
	if err := aw.Close(); err == nil {
		t.Fatal("Close() succeeded, want an error")
	}
	assertNotExist(t, path+".idx_")
}

// TestNewFailureLeavesNoFiles verifies that a failing New removes whatever it
// created: neither the avi file nor the temporary index file may remain.
func TestNewFailureLeavesNoFiles(t *testing.T) {
	t.Run("header write failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "hdrfail.avi")
		_, err := newAviWriter(path, 32, 24, 5, func(aw *aviWriter) {
			aw.writeHook = func(p []byte) (int, error) {
				return 0, errors.New("simulated write failure")
			}
		})
		if err == nil {
			t.Fatal("New() succeeded, want an error")
		}
		assertNotExist(t, path)
		assertNotExist(t, path+".idx_")
	})

	t.Run("avi create failure", func(t *testing.T) {
		dir := t.TempDir()
		// os.Create fails if the path is an existing directory.
		path := filepath.Join(dir, "isdir.avi")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := New(path, 32, 24, 5); err == nil {
			t.Fatal("New() succeeded, want an error")
		}
		assertNotExist(t, path+".idx_")
	})

	t.Run("index create failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "idxfail.avi")
		// Make creation of the index file fail: a directory occupies its path.
		if err := os.Mkdir(path+".idx_", 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := New(path, 32, 24, 5); err == nil {
			t.Fatal("New() succeeded, want an error")
		}
		// The avi file created before the failure must be removed.
		assertNotExist(t, path)
	})
}

// TestErrTooLargeIsNotSticky verifies that a frame rejected because the file
// would get too large does not prevent writing further frames.
func TestErrTooLargeDoesNotCorruptFile(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)
	if err := aw.AddFrame(frameData(1, 100)); err != nil {
		t.Fatalf("AddFrame failed: %v", err)
	}
	// Pretend a huge number of frames was already written so the limit trips.
	aw.mu.Lock()
	aw.frames = int((4200000000 - 100) / 16)
	aw.mu.Unlock()
	if err := aw.AddFrame(frameData(2, 100)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("AddFrame = %v, want ErrTooLarge", err)
	}
	aw.mu.Lock()
	aw.frames = 1
	aw.mu.Unlock()
	if err := aw.AddFrame(frameData(3, 100)); err != nil {
		t.Fatalf("AddFrame after ErrTooLarge failed: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)
	if len(p.frameChunk) != 2 {
		t.Errorf("frame count = %d, want 2", len(p.frameChunk))
	}
}

// TestFrameFailureRollbackUnderConcurrency ensures a failure in one goroutine
// does not corrupt the file for frames written by other goroutines.
func TestFrameFailureUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	aw, path := mustNew(t, dir)

	bad := frameData(99, 111)
	aw.writeHook = func(p []byte) (int, error) {
		if bytes.Contains(p, bad) {
			return 0, errors.New("simulated disk failure")
		}
		return aw.avif.Write(p)
	}

	const goodFrames = 40
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < goodFrames; i++ {
		wg.Add(1)
		go func(tag int) {
			defer wg.Done()
			err := aw.AddFrame(frameData(tag+1, 150))
			if err != nil {
				// Once the bad frame fails, the failure is sticky; frames that
				// are submitted afterwards must fail too.
				if !strings.Contains(err.Error(), "simulated disk failure") {
					t.Errorf("good AddFrame(%d) failed with unexpected error: %v", tag, err)
				}
				return
			}
			mu.Lock()
			succeeded++
			mu.Unlock()
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := aw.AddFrame(bad); err == nil {
			t.Error("AddFrame(bad) succeeded, want an error")
		}
	}()
	wg.Wait()

	if err := aw.Close(); err == nil {
		t.Error("Close() after a frame failure returned nil, want an error")
	}
	assertNotExist(t, path+".idx_")

	p := parseAVI(t, readFile(t, path))
	p.verifyConsistent(t)
	if len(p.frameChunk) != succeeded {
		t.Fatalf("frame count = %d, want %d (successful AddFrames)", len(p.frameChunk), succeeded)
	}
	for i, payload := range p.framePayloads() {
		if len(payload) != 150 {
			t.Fatalf("frame %d in file has length %d, want 150", i, len(payload))
		}
	}
}
