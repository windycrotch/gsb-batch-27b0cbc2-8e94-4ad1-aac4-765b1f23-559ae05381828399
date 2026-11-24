package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"
)

// readWithTimeout fails the test if a Read does not return within the
// deadline, turning a deadlock into a test failure instead of a hang.
func readWithTimeout(t *testing.T, s *OfflineStream, dst []int32) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := s.Read(dst)
		ch <- result{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("OfflineStream.Read blocked past deadline (lost wakeup or deadlock)")
		return 0, nil
	}
}

func TestOfflineWriteReadOrderAndEOF(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	want := []int32{10, 20, 30, 40, 50}
	if err := s.WriteValues(want); err != nil {
		t.Fatalf("WriteValues: %v", err)
	}
	s.Close()

	got := make([]int32, 3)
	n, err := readWithTimeout(t, s, got)
	if err != nil || n != 3 {
		t.Fatalf("first Read: n=%d err=%v", n, err)
	}
	if !equalInt32(got, want[:3]) {
		t.Fatalf("first Read = %v, want %v", got, want[:3])
	}

	// Asking for more than remains must return the buffered partial chunk,
	// not block and not EOF while data remains.
	n, err = readWithTimeout(t, s, got)
	if err != nil || n != 2 {
		t.Fatalf("partial Read: n=%d err=%v", n, err)
	}
	if !equalInt32(got[:2], want[3:]) {
		t.Fatalf("partial Read = %v, want %v", got[:2], want[3:])
	}

	n, err = readWithTimeout(t, s, got)
	if n != 0 || err != io.EOF {
		t.Fatalf("drained Read: n=%d err=%v, want 0, io.EOF", n, err)
	}
	// EOF is stable: further reads keep returning EOF.
	n, err = readWithTimeout(t, s, got)
	if n != 0 || err != io.EOF {
		t.Fatalf("second EOF Read: n=%d err=%v", n, err)
	}
}

func TestOfflineWriteAfterCloseIsRejected(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	if err := s.WriteValues([]int32{1, 2, 3}); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	s.Close()
	s.Close() // must be a safe, idempotent no-op

	if err := s.WriteValues([]int32{4, 5}); !errors.Is(err, ErrOfflineStreamClosed) {
		t.Fatalf("WriteValues after Close = %v, want ErrOfflineStreamClosed", err)
	}

	// The rejected write must not have mutated the buffer.
	got := make([]int32, 3)
	n, err := readWithTimeout(t, s, got)
	if err != nil || n != 3 || !equalInt32(got, []int32{1, 2, 3}) {
		t.Fatalf("buffer changed by rejected write: n=%d err=%v got=%v", n, err, got)
	}
	if _, err := readWithTimeout(t, s, got); err != io.EOF {
		t.Fatalf("want EOF after drain, got %v", err)
	}
}

func TestOfflineWriteBytesAfterClose(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	s.Close()
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[0:], 42)
	binary.LittleEndian.PutUint32(raw[4:], 43)
	if err := s.WriteBytes(raw, binary.LittleEndian, Int32); !errors.Is(err, ErrOfflineStreamClosed) {
		t.Fatalf("WriteBytes after Close = %v, want ErrOfflineStreamClosed", err)
	}
	dst := make([]int32, 1)
	if _, err := readWithTimeout(t, s, dst); err != io.EOF {
		t.Fatalf("want EOF on empty closed stream, got %v", err)
	}
}

func TestOfflineBlockedReaderWokenByWriteAndClose(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	dst := make([]int32, 4)

	// Reader starts first, before any data exists.
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 2)
	go func() {
		n, err := s.Read(dst)
		ch <- res{n, err}
	}()

	time.Sleep(50 * time.Millisecond) // let the reader block
	if err := s.WriteValues([]int32{1, 2, 3, 4}); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case r := <-ch:
		if r.err != nil || r.n != 4 || !equalInt32(dst, []int32{1, 2, 3, 4}) {
			t.Fatalf("woken Read: n=%d err=%v dst=%v", r.n, r.err, dst)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader was not woken by Write (lost data event)")
	}

	// A blocked reader must also be released by Close, yielding EOF once
	// nothing remains.
	go func() {
		n, err := s.Read(make([]int32, 2))
		ch <- res{n, err}
	}()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case r := <-ch:
		if r.n != 0 || r.err != io.EOF {
			t.Fatalf("close wake: n=%d err=%v, want 0/EOF", r.n, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader was not woken by Close (lost close event)")
	}
}

func TestOfflineConcurrentReadersNoDuplicationNoLoss(t *testing.T) {
	s := NewOfflineStream(8000, 64)
	const total = 2000
	for i := 0; i < total; i++ {
		if err := s.WriteValues([]int32{int32(i + 1)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	s.Close()

	const readers = 8
	var mu sync.Mutex
	consumed := make([]int32, 0, total)
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Vary destination sizes, including 1, to exercise partial reads.
			dst := make([]int32, 1+id*3)
			for {
				n, err := s.Read(dst)
				mu.Lock()
				consumed = append(consumed, dst[:n]...)
				mu.Unlock()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Errorf("reader %d unexpected err: %v", id, err)
					return
				}
			}
		}(r)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent readers did not all reach EOF")
	}

	if len(consumed) != total {
		t.Fatalf("consumed %d samples, want %d (loss or duplication)", len(consumed), total)
	}
	got := append([]int32(nil), consumed...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i := 0; i < total; i++ {
		if got[i] != int32(i+1) {
			t.Fatalf("sample multiset mismatch at %d: got %d, want %d", i, got[i], i+1)
		}
	}
}

func TestOfflineConcurrentWritersAndReaders(t *testing.T) {
	s := NewOfflineStream(8000, 128)
	const writers = 6
	const perWriter = 500
	const readers = 4

	want := make(map[int32]int)
	for w := 0; w < writers; w++ {
		base := w * perWriter
		for i := 0; i < perWriter; i++ {
			want[int32(base+i+1)]++
		}
	}

	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(id int) {
			defer writersWG.Done()
			base := id * perWriter
			buf := make([]int32, 7)
			for i := 0; i < perWriter; i++ {
				buf[0] = int32(base + i + 1)
				if err := s.WriteValues(buf[:1]); err != nil {
					t.Errorf("writer %d: %v", id, err)
					return
				}
			}
		}(w)
	}

	var mu sync.Mutex
	consumed := make([]int32, 0, writers*perWriter)
	var readersWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func(id int) {
			defer readersWG.Done()
			dst := make([]int32, 2+id*2)
			for {
				n, err := s.Read(dst)
				mu.Lock()
				consumed = append(consumed, dst[:n]...)
				mu.Unlock()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Errorf("reader %d: %v", id, err)
					return
				}
			}
		}(r)
	}

	go func() {
		writersWG.Wait()
		s.Close()
	}()

	done := make(chan struct{})
	go func() { readersWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("readers did not all finish after writers closed the stream")
	}

	if len(consumed) != len(want) {
		t.Fatalf("consumed %d, want %d", len(consumed), len(want))
	}
	got := make(map[int32]int)
	for _, v := range consumed {
		got[v]++
	}
	for v, c := range want {
		if got[v] != c {
			t.Fatalf("sample %d count = %d, want %d", v, got[v], c)
		}
	}
}

// errSource is a reader that hands out one chunk and then fails.
type errSource struct {
	data []byte
	err  error
	done bool
}

func (e *errSource) Read(p []byte) (int, error) {
	if e.done {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.done = true
	return n, e.err
}

func TestOfflineReadBytesPropagatesUpstreamError(t *testing.T) {
	sentinel := errors.New("boom: upstream exploded")
	raw := make([]byte, 8)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	src := &errSource{data: raw, err: sentinel}
	s := NewOfflineStream(8000, 16)

	done := make(chan error, 1)
	go func() { done <- s.ReadBytes(src, binary.LittleEndian, Int8) }()

	// All eight buffered samples must come out in order before the error is
	// observable. Read them in two differently sized requests to exercise
	// the partial-drain path as well.
	first := make([]int32, 3)
	n, err := readWithTimeout(t, s, first)
	if err != nil || n != 3 {
		t.Fatalf("first drain Read: n=%d err=%v", n, err)
	}
	if first[0] != int32(1)<<24 || first[2] != int32(3)<<24 {
		t.Fatalf("buffered data mismatch: %v", first)
	}

	rest := make([]int32, 8)
	n, err = readWithTimeout(t, s, rest)
	if err != nil || n != 5 {
		t.Fatalf("second drain Read: n=%d err=%v", n, err)
	}
	for i := 0; i < 5; i++ {
		if rest[i] != int32(i+4)<<24 {
			t.Fatalf("sample %d mismatch: %v", i, rest[:5])
		}
	}

	// Now that the buffer is empty, the upstream error must reach waiting
	// readers instead of blocking them forever.
	n, err = readWithTimeout(t, s, rest)
	if n != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("error Read: n=%d err=%v, want sentinel", n, err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Fatalf("ReadBytes returned %v, want sentinel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBytes did not return after upstream error")
	}

	// Writes after an error-finished stream are rejected, and the terminal
	// error remains observable to further readers.
	if err := s.WriteValues([]int32{99}); !errors.Is(err, ErrOfflineStreamClosed) {
		t.Fatalf("write after error finish = %v", err)
	}
	if _, err := readWithTimeout(t, s, rest); !errors.Is(err, sentinel) {
		t.Fatalf("terminal error not sticky: %v", err)
	}
}

func TestOfflineReadBytesEOF(t *testing.T) {
	raw := make([]byte, 8) // two little-endian int32 samples
	binary.LittleEndian.PutUint32(raw[0:], 1234)
	binary.LittleEndian.PutUint32(raw[4:], 5678)
	s := NewOfflineStream(8000, 16)

	finished := make(chan error, 1)
	go func() {
		finished <- s.ReadBytes(bytes.NewReader(raw), binary.LittleEndian, Int32)
	}()

	dst := make([]int32, 2)
	n, err := readWithTimeout(t, s, dst)
	if err != nil || n != 2 || dst[0] != 1234 || dst[1] != 5678 {
		t.Fatalf("Read: n=%d err=%v dst=%v", n, err, dst)
	}
	if _, err := readWithTimeout(t, s, dst); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	select {
	case err := <-finished:
		if err != io.EOF {
			t.Fatalf("ReadBytes = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBytes did not finish on EOF source")
	}
}

func TestOfflineWriteBytesNumberTypes(t *testing.T) {
	cases := []struct {
		numType NumberType
		raw     []byte
		want    []int32
	}{
		{Int8, []byte{1, 0xFE}, []int32{int32(int8(1)) << 24, int32(int8(-2)) << 24}},
		{Int16, u16le(1000, -2000), []int32{int32(int16(1000)) << 16, int32(int16(-2000)) << 16}},
		{Int32, u32le(70000, -70000), []int32{70000, -70000}},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			s := NewOfflineStream(8000, 16)
			if err := s.WriteBytes(tc.raw, binary.LittleEndian, tc.numType); err != nil {
				t.Fatalf("WriteBytes: %v", err)
			}
			s.Close()
			dst := make([]int32, len(tc.want)+1)
			n, err := readWithTimeout(t, s, dst)
			if err != nil || n != len(tc.want) {
				t.Fatalf("Read: n=%d err=%v", n, err)
			}
			if !equalInt32(dst[:n], tc.want) {
				t.Fatalf("got %v want %v", dst[:n], tc.want)
			}
		})
	}
}

func TestOfflineInvalidNumberType(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	err := s.WriteBytes([]byte{1, 2}, binary.LittleEndian, NumberType(99))
	if !errors.Is(err, ErrInvalidNumberType) {
		t.Fatalf("got %v, want ErrInvalidNumberType", err)
	}
}

// blockingReader sleeps until released, letting the test Close the stream
// while ReadBytes is parked inside a source read.
type blockingReader struct {
	release chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func TestOfflineCloseDuringReadBytes(t *testing.T) {
	s := NewOfflineStream(8000, 16)
	if err := s.WriteValues([]int32{7, 8, 9}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	rd := &blockingReader{release: make(chan struct{})}
	readBytesDone := make(chan error, 1)
	go func() { readBytesDone <- s.ReadBytes(rd, binary.LittleEndian, Int16) }()

	time.Sleep(50 * time.Millisecond) // ensure ReadBytes is parked in rd.Read
	s.Close()
	close(rd.release)

	select {
	case err := <-readBytesDone:
		// The source unblocks with EOF after the Close; ReadBytes must
		// terminate (either reporting the closed stream or the source EOF)
		// and must never have appended anything after the Close.
		if err == nil {
			t.Fatal("ReadBytes returned nil error on a closed stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBytes did not stop after Close")
	}

	// Pre-close data is intact and drains to EOF.
	dst := make([]int32, 3)
	n, err := readWithTimeout(t, s, dst)
	if err != nil || n != 3 || !equalInt32(dst, []int32{7, 8, 9}) {
		t.Fatalf("drain: n=%d err=%v dst=%v", n, err, dst)
	}
	if _, err := readWithTimeout(t, s, dst); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func equalInt32(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func u16le(vals ...int16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func u32le(vals ...int32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], uint32(v))
	}
	return b
}
