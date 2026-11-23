package mp4

import (
	"errors"
	"testing"

	"github.com/alfg/mp4/atom"
)

func u32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func makeBox(name string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	copy(b[0:4], u32(uint32(len(b))))
	copy(b[4:8], name)
	copy(b[8:], payload)
	return b
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// buildFile builds a minimal but valid MP4: ftyp + moov(mvhd +
// trak(tkhd + mdia(mdhd + hdlr + minf(vmhd + stbl(stts + stsd(avc1)))))).
// sttsCount is written into the stts entry-count field verbatim so tests can
// corrupt it.
func buildFile(sttsCount uint32) []byte {
	ftyp := makeBox("ftyp", cat([]byte("isom"), u32(0x200), []byte("isomiso2")))

	mvhdPayload := make([]byte, 26)
	copy(mvhdPayload[12:16], u32(1000)) // timescale
	copy(mvhdPayload[16:20], u32(2000)) // duration
	mvhd := makeBox("mvhd", mvhdPayload)

	tkhd := makeBox("tkhd", make([]byte, 84))

	mdhdPayload := make([]byte, 22)
	copy(mdhdPayload[12:16], u32(44100))
	mdhd := makeBox("mdhd", mdhdPayload)

	hdlrPayload := make([]byte, 24)
	copy(hdlrPayload[8:12], "vide")
	hdlr := makeBox("hdlr", hdlrPayload)

	vmhd := makeBox("vmhd", make([]byte, 8))

	sttsPayload := cat(make([]byte, 4), u32(sttsCount), u32(100), u32(40))
	stts := makeBox("stts", sttsPayload)

	avc1 := makeBox("avc1", []byte{0})
	stsd := makeBox("stsd", cat(make([]byte, 8), avc1))

	stbl := makeBox("stbl", cat(stts, stsd))
	minf := makeBox("minf", cat(vmhd, stbl))
	mdia := makeBox("mdia", cat(mdhd, hdlr, minf))
	trak := makeBox("trak", cat(tkhd, mdia))
	moov := makeBox("moov", cat(mvhd, trak))

	return cat(ftyp, moov)
}

func TestOpenFromBytesValid(t *testing.T) {
	f, err := OpenFromBytes(buildFile(1))
	if err != nil {
		t.Fatalf("valid file returned error: %v", err)
	}
	if f.Ftyp == nil || f.Ftyp.MajorBrand != "isom" {
		t.Error("ftyp not parsed")
	}
	if f.Moov == nil || f.Moov.Mvhd == nil || f.Moov.Mvhd.Timescale != 1000 {
		t.Error("moov/mvhd not parsed")
	}
	stts := f.Moov.Traks[0].Mdia.Minf.Stbl.Stts
	if stts == nil || len(stts.SampleCounts) != 1 || stts.SampleCounts[0] != 100 {
		t.Error("stts not parsed")
	}
	if len(f.Errors) != 0 {
		t.Errorf("unexpected box errors: %v", f.Errors)
	}
}

func TestCorruptSttsEntryCount(t *testing.T) {
	f, err := OpenFromBytes(buildFile(0xFFFFFFFF))
	if err == nil {
		t.Fatal("expected partial error")
	}
	if f == nil {
		t.Fatal("reader must be returned on partial failure")
	}
	if !atom.IsPartial(err) {
		t.Fatalf("expected PartialError, got %T: %v", err, err)
	}

	// The bad box is isolated: everything else stays available.
	if f.Ftyp == nil || f.Moov == nil || f.Moov.Mvhd == nil {
		t.Error("ftyp/moov/mvhd must survive a corrupt stts")
	}
	trak := f.Moov.Traks[0]
	if trak.Tkhd == nil || trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
		t.Error("trak/mdia/minf/stbl must survive a corrupt stts")
	}
	if trak.Mdia.Minf.Stbl.Stts != nil {
		t.Error("corrupt stts must not be exposed")
	}
	if trak.Mdia.Minf.Stbl.Stsd == nil || trak.Mdia.Minf.Stbl.Stsd.Avc1 == nil {
		t.Error("stsd sibling must survive a corrupt stts")
	}

	if len(f.Errors) != 1 {
		t.Fatalf("expected exactly 1 box error, got %d: %v", len(f.Errors), f.Errors)
	}
	be := f.Errors[0]
	if be.Name != "stts" {
		t.Errorf("error box name = %q, want stts", be.Name)
	}
	if be.Offset <= 0 {
		t.Errorf("error box offset = %d, want > 0", be.Offset)
	}
}

func TestCorruptBoxSizeTooSmall(t *testing.T) {
	good := buildFile(1)
	ftypLen := 8 + 16
	// Insert a box whose declared size (2) is smaller than the header.
	bad := cat(u32(2), []byte("free"), make([]byte, 6))
	data := cat(good[:ftypLen], bad, good[ftypLen:])

	f, err := OpenFromBytes(data)
	if !atom.IsPartial(err) {
		t.Fatalf("expected partial error, got %v", err)
	}
	// The parser must resynchronize and still find moov.
	if f.Moov == nil || f.Moov.Mvhd == nil {
		t.Error("moov must be found after a corrupt sibling box")
	}
	if len(f.Errors) == 0 {
		t.Fatal("corrupt box must be recorded")
	}
	if f.Errors[0].Offset != int64(ftypLen) {
		t.Errorf("error offset = %d, want %d", f.Errors[0].Offset, ftypLen)
	}
}

func TestCorruptBoxSizeBeyondEOF(t *testing.T) {
	good := buildFile(1)
	bad := cat(u32(0x7FFFFFFF), []byte("mdat"), make([]byte, 4))
	data := cat(good, bad)

	f, err := OpenFromBytes(data)
	if !atom.IsPartial(err) {
		t.Fatalf("expected partial error, got %v", err)
	}
	if f.Ftyp == nil || f.Moov == nil {
		t.Error("valid boxes must survive a trailing corrupt box")
	}
	if len(f.Errors) != 1 || f.Errors[0].Name != "mdat" {
		t.Fatalf("unexpected errors: %v", f.Errors)
	}
}

func TestCorruptElstEntryCount(t *testing.T) {
	f, err := OpenFromBytes(buildFileWithEdts())
	if !atom.IsPartial(err) {
		t.Fatalf("expected partial error, got %v", err)
	}
	found := false
	for _, be := range f.Errors {
		if be.Name == "elst" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected elst error, got %v", f.Errors)
	}
	if f.Moov == nil || f.Moov.Traks[0].Mdia == nil {
		t.Error("corrupt elst must not take down the track")
	}
}

func buildFileWithEdts() []byte {
	ftyp := makeBox("ftyp", cat([]byte("isom"), u32(0x200), []byte("isomiso2")))
	mvhd := makeBox("mvhd", make([]byte, 26))
	tkhd := makeBox("tkhd", make([]byte, 84))
	elst := makeBox("elst", cat(make([]byte, 4), u32(0x00FFFFFF)))
	edts := makeBox("edts", elst)
	mdhd := makeBox("mdhd", make([]byte, 22))
	mdia := makeBox("mdia", mdhd)
	trak := makeBox("trak", cat(tkhd, edts, mdia))
	moov := makeBox("moov", cat(mvhd, trak))
	return cat(ftyp, moov)
}

func TestGarbageInputFails(t *testing.T) {
	for _, data := range [][]byte{
		{},
		[]byte("not an mp4 file at all"),
		{0, 0, 0, 2, 'f', 't', 'y', 'p'},
	} {
		f, err := OpenFromBytes(data)
		if err == nil {
			t.Errorf("input %q: expected error", data)
		}
		if f != nil {
			t.Errorf("input %q: reader must be nil on total failure", data)
		}
		if !errors.Is(err, atom.ErrNoValidBoxes) {
			t.Errorf("input %q: error = %v, want ErrNoValidBoxes", data, err)
		}
	}
}

func TestDeterministicRepeatedParse(t *testing.T) {
	data := buildFile(0xFFFFFFFF)

	f1, err1 := OpenFromBytes(data)
	f2, err2 := OpenFromBytes(data)

	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("inconsistent errors: %v vs %v", err1, err2)
	}
	if len(f1.Errors) != len(f2.Errors) {
		t.Fatalf("inconsistent error counts: %d vs %d", len(f1.Errors), len(f2.Errors))
	}
	for i := range f1.Errors {
		if f1.Errors[i].Name != f2.Errors[i].Name || f1.Errors[i].Offset != f2.Errors[i].Offset {
			t.Errorf("error %d differs: %v vs %v", i, f1.Errors[i], f2.Errors[i])
		}
	}
	if f1.Ftyp.MajorBrand != f2.Ftyp.MajorBrand ||
		f1.Moov.Mvhd.Timescale != f2.Moov.Mvhd.Timescale {
		t.Error("parsed fields differ between runs")
	}
	// No shared state: mutating one reader's error list must not affect the other.
	if &f1.Errors[0] == &f2.Errors[0] {
		t.Error("readers share error storage")
	}
}
