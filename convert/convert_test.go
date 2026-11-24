package convert_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/qiniu/audio"
	"github.com/qiniu/audio/convert"
	_ "github.com/qiniu/audio/wav"
)

// buildWAV builds a PCM wav file in memory.
func buildWAV(channels, bitsPerSample, sampleRate int, data []byte) []byte {
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign
	var b bytes.Buffer
	b.WriteString("RIFF")
	writeU32(&b, uint32(36+len(data)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	writeU32(&b, 16)
	writeU16(&b, 1) // PCM
	writeU16(&b, uint16(channels))
	writeU32(&b, uint32(sampleRate))
	writeU32(&b, uint32(byteRate))
	writeU16(&b, uint16(blockAlign))
	writeU16(&b, uint16(bitsPerSample))
	b.WriteString("data")
	writeU32(&b, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func writeU16(b *bytes.Buffer, v uint16) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
}

func writeU32(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v >> 16))
	b.WriteByte(byte(v >> 24))
}

func decodeWAV(t *testing.T, data []byte) audio.Decoded {
	t.Helper()
	d, format, err := audio.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if format != "wav" {
		t.Fatalf("format = %q, want wav", format)
	}
	return d
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return out
}

// mono16ToStereo16 is the reference conversion for 16-bit mono -> 16-bit stereo.
func mono16ToStereo16(src []byte) []byte {
	out := make([]byte, 0, len(src)*2)
	for i := 0; i+1 < len(src); i += 2 {
		out = append(out, src[i], src[i+1], src[i], src[i+1])
	}
	return out
}

// eightToStereo16 is the reference conversion for 8-bit -> 16-bit stereo.
// mono: each byte becomes 4 bytes; stereo: each byte pair becomes 4 bytes.
func eightTo16(v byte) (lo, hi uint8) {
	s := int16(int(v)*0x101 - (1 << 15))
	return uint8(s), uint8(s >> 8)
}

func mono8ToStereo16(src []byte) []byte {
	out := make([]byte, 0, len(src)*4)
	for _, v := range src {
		lo, hi := eightTo16(v)
		out = append(out, lo, hi, lo, hi)
	}
	return out
}

func stereo8ToStereo16(src []byte) []byte {
	out := make([]byte, 0, len(src)*2)
	for i := 0; i+1 < len(src); i += 2 {
		lo0, hi0 := eightTo16(src[i])
		lo1, hi1 := eightTo16(src[i+1])
		out = append(out, lo0, hi0, lo1, hi1)
	}
	return out
}

func makeSamples(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*37 + 11)
	}
	return data
}

func TestStereo16SequentialRead(t *testing.T) {
	cases := []struct {
		name    string
		ch      int
		bits    int
		data    []byte
		convert func([]byte) []byte
	}{
		{"mono16", 1, 16, makeSamples(4098), mono16ToStereo16},
		{"mono8", 1, 8, makeSamples(2049), mono8ToStereo16},
		{"stereo8", 2, 8, makeSamples(4098), stereo8ToStereo16},
		{"stereo16-passthrough", 2, 16, makeSamples(4100), func(b []byte) []byte { return b }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decodeWAV(t, buildWAV(c.ch, c.bits, 44100, c.data))
			s := convert.ToStereo16(d)
			got := readAll(t, s)
			want := c.convert(c.data)
			if !bytes.Equal(got, want) {
				t.Fatalf("sequential read mismatch: got %d bytes, want %d bytes", len(got), len(want))
			}
			if s.Length() != int64(len(want)) {
				t.Fatalf("Length = %d, want %d", s.Length(), len(want))
			}
		})
	}
}

func TestStereo16SeekConsistency(t *testing.T) {
	cases := []struct {
		name string
		ch   int
		bits int
		data []byte
	}{
		{"mono16", 1, 16, makeSamples(8192)},
		{"mono8", 1, 8, makeSamples(4099)},
		{"stereo8", 2, 8, makeSamples(8190)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decodeWAV(t, buildWAV(c.ch, c.bits, 44100, c.data))
			s := convert.ToStereo16(d)
			full := readAll(t, s)

			length := s.Length()
			if length != int64(len(full)) {
				t.Fatalf("Length = %d, want %d", length, len(full))
			}

			// Random seeks must match sequential content. Offsets must be
			// frame-aligned (4 bytes in the stereo16 stream).
			for _, off := range []int64{0, 4, 100, 1024, 4096, 5000, length / 2, length - 8} {
				off = off / 4 * 4
				if off < 0 || off > length {
					continue
				}
				pos, err := s.Seek(off, io.SeekStart)
				if err != nil {
					t.Fatalf("Seek(%d): %v", off, err)
				}
				if pos != off {
					t.Fatalf("Seek(%d) returned %d", off, pos)
				}
				buf := make([]byte, 512)
				n, err := s.Read(buf)
				if err != nil && err != io.EOF {
					t.Fatalf("Read after Seek(%d): %v", off, err)
				}
				end := off + int64(n)
				if end > length {
					end = length
				}
				if !bytes.Equal(buf[:n], full[off:end]) {
					t.Fatalf("data at offset %d differs from sequential read", off)
				}
				// Repeated seek to the same position must give identical data.
				if _, err := s.Seek(off, io.SeekStart); err != nil {
					t.Fatalf("re-Seek(%d): %v", off, err)
				}
				buf2 := make([]byte, 512)
				n2, _ := s.Read(buf2)
				if n2 != n || !bytes.Equal(buf2[:n2], buf[:n]) {
					t.Fatalf("repeated Seek(%d) + Read gave different data", off)
				}
			}

			// SeekCurrent must be relative to the logical position.
			if _, err := s.Seek(1024, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			pos, err := s.Seek(512, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			if pos != 1536 {
				t.Fatalf("SeekCurrent pos = %d, want 1536", pos)
			}
			buf := make([]byte, 256)
			n, _ := s.Read(buf)
			if !bytes.Equal(buf[:n], full[1536:1536+n]) {
				t.Fatal("SeekCurrent landed on wrong data")
			}

			// Seek to end: only EOF allowed afterwards.
			if _, err := s.Seek(0, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			if n, err := s.Read(make([]byte, 64)); n != 0 || err != io.EOF {
				t.Fatalf("Read at end = (%d, %v), want (0, EOF)", n, err)
			}
			if n, err := s.Read(make([]byte, 64)); n != 0 || err != io.EOF {
				t.Fatalf("second Read at end = (%d, %v), want (0, EOF)", n, err)
			}
		})
	}
}

func TestResampleCoversAllSamples(t *testing.T) {
	// 5000 stereo frames: not a multiple of the internal 4096-frame block.
	frames := 5000
	data := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		v := int16(i * 7)
		data[4*i] = byte(v)
		data[4*i+1] = byte(v >> 8)
		data[4*i+2] = byte(v)
		data[4*i+3] = byte(v >> 8)
	}
	for _, to := range []int{22050, 48000} {
		d := decodeWAV(t, buildWAV(2, 16, 44100, data))
		r := convert.Resample(d, to)
		got := readAll(t, r)
		wantLen := r.Length()
		if int64(len(got)) != wantLen {
			t.Fatalf("resample to %d: got %d bytes, Length = %d", to, len(got), wantLen)
		}
		// Output must cover all source samples: expected output frames is
		// frames*to/44100 (rounded down to frame alignment).
		wantFrames := int64(frames) * int64(to) / 44100
		gotFrames := int64(len(got)) / 4
		if gotFrames < wantFrames {
			t.Fatalf("resample to %d: got %d frames, want at least %d (tail lost)", to, gotFrames, wantFrames)
		}
	}
}

func TestResampleSeekStability(t *testing.T) {
	frames := 9000 // crosses the 4096 block boundary
	data := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		v := int16(i*13 - 2000)
		data[4*i] = byte(v)
		data[4*i+1] = byte(v >> 8)
		data[4*i+2] = byte(v)
		data[4*i+3] = byte(v >> 8)
	}
	d := decodeWAV(t, buildWAV(2, 16, 44100, data))
	r := convert.Resample(d, 48000)

	// Ground truth: sequential read of the whole stream.
	full := readAll(t, r)

	length := r.Length()
	for _, off := range []int64{0, 4096 * 4, 4096*4 - 4, 4096 * 4 * 2, length / 3, length - 16} {
		if off < 0 || off > length {
			continue
		}
		var prev []byte
		for rep := 0; rep < 3; rep++ {
			pos, err := r.Seek(off, io.SeekStart)
			if err != nil {
				t.Fatalf("Seek(%d): %v", off, err)
			}
			if pos != off {
				t.Fatalf("Seek(%d) = %d", off, pos)
			}
			buf := make([]byte, 1024)
			n, err := r.Read(buf)
			if err != nil && err != io.EOF {
				t.Fatalf("Read after Seek(%d): %v", off, err)
			}
			got := buf[:n]
			end := off + int64(n)
			if end > length {
				end = length
			}
			if !bytes.Equal(got, full[off:end]) {
				t.Fatalf("rep %d: data at %d differs from sequential read", rep, off)
			}
			if prev != nil && !bytes.Equal(got, prev) {
				t.Fatalf("rep %d: repeated Seek(%d)+Read unstable", rep, off)
			}
			prev = got
		}
	}

	// SeekEnd must be absolute, not accumulative.
	pos1, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos1 != length {
		t.Fatalf("Seek(0, SeekEnd) = %d, want %d", pos1, length)
	}
	pos2, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos2 != length {
		t.Fatalf("second Seek(0, SeekEnd) = %d, want %d", pos2, length)
	}
	pos3, err := r.Seek(-16, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos3 != length-16 {
		t.Fatalf("Seek(-16, SeekEnd) = %d, want %d", pos3, length-16)
	}
	if n, err := r.Read(make([]byte, 64)); n != 16 && err != io.EOF {
		t.Fatalf("Read after Seek(-16, SeekEnd) = (%d, %v), want 16 bytes", n, err)
	}
	// At end: only EOF.
	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 64)); n != 0 || err != io.EOF {
		t.Fatalf("Read at end = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestConcurrentSeekRead(t *testing.T) {
	data := makeSamples(16384)
	d := decodeWAV(t, buildWAV(1, 16, 44100, data))
	s := convert.ToStereo16(d)
	r := convert.Resample(s, 22050)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			buf := make([]byte, 256)
			for i := 0; i < 50; i++ {
				off := int64((g*131 + i*257) % 4096)
				off = off / 4 * 4
				if _, err := r.Seek(off, io.SeekStart); err != nil {
					t.Errorf("Seek: %v", err)
					return
				}
				if _, err := r.Read(buf); err != nil && err != io.EOF {
					t.Errorf("Read: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestDecodeAfterError(t *testing.T) {
	good := buildWAV(2, 16, 44100, makeSamples(1024))
	// Truncated inside the header: decode must fail with a recognizable
	// error, no panic.
	bad := good[:30]
	if _, _, err := audio.Decode(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for truncated wav")
	}
	// Re-decoding the same (good) input after a failure must behave like the
	// first successful path.
	r := bytes.NewReader(good)
	d1, _, err1 := audio.Decode(r)
	// Force an error path on another decoder instance over the same input kind.
	if _, _, err := audio.Decode(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for truncated wav")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	d2, _, err2 := audio.Decode(r)
	if err1 != nil || err2 != nil {
		t.Fatalf("decode good input: %v / %v", err1, err2)
	}
	a := readAll(t, d1)
	b := readAll(t, d2)
	if !bytes.Equal(a, b) {
		t.Fatal("re-decode after error gave different data")
	}
}

func TestWavStreamSeekCurrent(t *testing.T) {
	data := makeSamples(2048)
	d := decodeWAV(t, buildWAV(2, 16, 44100, data))

	// Read ahead so the buffered reader holds read-ahead data, then use
	// SeekCurrent: it must be relative to the logical read position.
	buf := make([]byte, 100)
	if _, err := io.ReadFull(d, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data[:100]) {
		t.Fatal("sequential read mismatch")
	}
	pos, err := d.Seek(4, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 104 {
		t.Fatalf("SeekCurrent pos = %d, want 104", pos)
	}
	b8 := make([]byte, 8)
	if _, err := io.ReadFull(d, b8); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b8, data[104:112]) {
		t.Fatalf("data after SeekCurrent = %v, want %v", b8, data[104:112])
	}
	// Negative SeekCurrent.
	pos, err = d.Seek(-12, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 100 {
		t.Fatalf("SeekCurrent(-12) pos = %d, want 100", pos)
	}
	if _, err := io.ReadFull(d, b8); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b8, data[100:108]) {
		t.Fatalf("data after SeekCurrent(-12) = %v, want %v", b8, data[100:108])
	}
	// SeekEnd then read: EOF only.
	if _, err := d.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Read(make([]byte, 16)); n != 0 || err != io.EOF {
		t.Fatalf("Read at end = (%d, %v), want (0, EOF)", n, err)
	}
	// Seek back and forth repeatedly: data must stay consistent.
	for rep := 0; rep < 3; rep++ {
		if _, err := d.Seek(512, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(d, b8); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b8, data[512:520]) {
			t.Fatalf("rep %d: data at 512 = %v, want %v", rep, b8, data[512:520])
		}
	}
}

func TestResampleNonAlignedSource(t *testing.T) {
	// Mono 8-bit wav with an odd data size: the source byte length seen by
	// the resampler is not a multiple of 4.
	data := makeSamples(4099)
	d := decodeWAV(t, buildWAV(1, 8, 44100, data))
	r := convert.Resample(d, 48000)
	got := readAll(t, r)
	if int64(len(got)) != r.Length() {
		t.Fatalf("got %d bytes, Length = %d", len(got), r.Length())
	}
	// Repeated seek to the same position must be stable.
	for rep := 0; rep < 2; rep++ {
		if _, err := r.Seek(1024, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, err := r.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(buf[:n], got[1024:1024+n]) {
			t.Fatalf("rep %d: data at 1024 differs from sequential read", rep)
		}
	}
}

// flakyRS injects a one-time read error that also advances the underlying
// reader, simulating a failed mid-block read.
type flakyRS struct {
	r      io.ReadSeeker
	failed bool
}

func (f *flakyRS) Read(p []byte) (int, error) {
	if !f.failed {
		f.failed = true
		f.r.Read(make([]byte, 100)) // partial advance, then error
		return 0, errInjected
	}
	return f.r.Read(p)
}

func (f *flakyRS) Seek(offset int64, whence int) (int64, error) {
	return f.r.Seek(offset, whence)
}

var errInjected = errors.New("injected read error")

func TestResampleReadErrorRecovery(t *testing.T) {
	frames := 5000
	data := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		v := int16(i * 5)
		data[4*i] = byte(v)
		data[4*i+1] = byte(v >> 8)
		data[4*i+2] = byte(v)
		data[4*i+3] = byte(v >> 8)
	}
	size := int64(len(data))

	// Ground truth from a clean source.
	clean := convert.NewResampling(bytes.NewReader(data), size, 44100, 48000)
	want := readAll(t, clean)

	// Same stream, but the first underlying read fails mid-block.
	flaky := &flakyRS{r: bytes.NewReader(data)}
	r := convert.NewResampling(flaky, size, 44100, 48000)
	buf := make([]byte, 4096)
	if _, err := r.Read(buf); err == nil {
		t.Fatal("expected injected error")
	}
	// Retrying must produce exactly the clean sequence, with no residue
	// from the failed read.
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, r)
	if !bytes.Equal(got, want) {
		t.Fatal("read after mid-stream error diverged from clean run")
	}
}
