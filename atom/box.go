package atom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// BoxHeaderSize Size of box header.
	BoxHeaderSize = 8
	UuidSize      = 16
)

// ErrNoValidBoxes is returned when parsing fails completely: not a single
// box could be identified (for example the file header is unreadable or the
// input is not an MP4 file at all).
var ErrNoValidBoxes = errors.New("mp4: no valid boxes could be parsed")

// BoxError describes a single box that failed to parse. The rest of the
// file is unaffected: the failed box only takes down itself and its subtree.
type BoxError struct {
	Name   string // Box type (fourcc). May be empty if the header was unreadable.
	Offset int64  // Absolute offset of the box in the file.
	Err    error  // Underlying cause.
}

func (e *BoxError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("box at offset %d: %v", e.Offset, e.Err)
	}
	return fmt.Sprintf("box %q at offset %d: %v", e.Name, e.Offset, e.Err)
}

// Unwrap returns the underlying cause.
func (e *BoxError) Unwrap() error { return e.Err }

// PartialError is returned by Mp4Reader.Parse when one or more boxes failed
// to parse but at least one box was identified. All successfully parsed
// boxes remain available on the Mp4Reader.
type PartialError struct {
	Errors []*BoxError
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("mp4: partial parse, %d box(es) failed: %v", len(e.Errors), e.Errors)
}

// IsPartial reports whether err indicates a partial parse: some boxes failed
// but the successfully parsed boxes are still usable.
func IsPartial(err error) bool {
	var pe *PartialError
	return errors.As(err, &pe)
}

// Mp4Reader defines an mp4 reader structure.
type Mp4Reader struct {
	Reader io.ReaderAt
	Ftyp   *FtypBox
	Moov   *MoovBox
	Mdat   *MdatBox
	Uuids  []*UuidBox
	Size   int64

	// Errors lists every box that failed to parse, in file order. It is
	// populated fresh by each Parse call and is never shared between readers.
	Errors []*BoxError

	IsFragmented bool
}

// Parse reads an MP4 reader for atom boxes.
//
// It returns nil on full success, a *PartialError when some boxes failed
// (the successfully parsed boxes remain available on the reader), and a
// fatal error (matching ErrNoValidBoxes) when no box could be read at all.
func (m *Mp4Reader) Parse() error {
	if m.Size == 0 {
		if ofile, ok := m.Reader.(*os.File); ok {
			info, err := ofile.Stat()
			if err != nil {
				return err
			}
			m.Size = info.Size()
		}
	}

	m.Errors = nil
	boxes := readBoxes(m, int64(0), m.Size)
	for _, box := range boxes {
		switch box.Name {
		case "ftyp":
			ftyp := &FtypBox{Box: box}
			if m.tryParse(box, ftyp.parse) {
				m.Ftyp = ftyp
			}
		case "mdat":
			m.Mdat = &MdatBox{Box: box}
		case "moov":
			moov := &MoovBox{Box: box}
			if m.tryParse(box, moov.parse) {
				m.Moov = moov
				m.IsFragmented = m.Moov.IsFragmented
			}
		case "uuid":
			uuidBox := &UuidBox{Box: box}
			if m.tryParse(box, uuidBox.parse) {
				m.Uuids = append(m.Uuids, uuidBox)
			}
		}
	}

	if len(boxes) == 0 {
		if len(m.Errors) > 0 {
			return fmt.Errorf("%w: %v", ErrNoValidBoxes, m.Errors[0])
		}
		return ErrNoValidBoxes
	}
	if len(m.Errors) > 0 {
		return &PartialError{Errors: m.Errors}
	}
	return nil
}

// addError records a failed box.
func (m *Mp4Reader) addError(name string, offset int64, err error) {
	m.Errors = append(m.Errors, &BoxError{Name: name, Offset: offset, Err: err})
}

// tryParse runs a box parse function, converting any error or panic into a
// recorded BoxError so a single corrupt box cannot abort or crash the whole
// parse. It reports whether the box parsed successfully.
func (m *Mp4Reader) tryParse(box *Box, fn func() error) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			m.addError(box.Name, box.Start, fmt.Errorf("panic while parsing: %v", r))
			ok = false
		}
	}()
	if err := fn(); err != nil {
		m.addError(box.Name, box.Start, err)
		return false
	}
	return true
}

// ReadBoxAt reads a box from an offset.
func (m *Mp4Reader) ReadBoxAt(offset int64) (boxSize uint32, boxType string) {
	size, name, err := m.readBoxHeader(offset, m.Size)
	if err != nil {
		return 0, ""
	}
	return size, name
}

// readBoxHeader reads and validates the 8-byte box header at offset.
// The box must fit inside [offset, end).
func (m *Mp4Reader) readBoxHeader(offset int64, end int64) (size uint32, name string, err error) {
	buf := m.ReadBytesAt(BoxHeaderSize, offset)
	if len(buf) < BoxHeaderSize {
		return 0, "", fmt.Errorf("cannot read %d-byte box header", BoxHeaderSize)
	}
	size = binary.BigEndian.Uint32(buf[0:4])
	name = string(buf[4:8])
	if size < BoxHeaderSize {
		return 0, name, fmt.Errorf("invalid box size %d (smaller than %d-byte header)", size, BoxHeaderSize)
	}
	if offset+int64(size) > end {
		return 0, name, fmt.Errorf("declared size %d exceeds containing box/file (offset %d, end %d)", size, offset, end)
	}
	return size, name, nil
}

// ReadBytesAt reads a box at n and offset.
func (m *Mp4Reader) ReadBytesAt(n int64, offset int64) (word []byte) {
	if n <= 0 || offset < 0 {
		return nil
	}
	buf := make([]byte, n)
	if _, err := m.Reader.ReadAt(buf, offset); err != nil {
		return nil
	}
	return buf
}

// Box defines an Atom Box structure.
type Box struct {
	Name        string
	Size, Start int64
	Reader      *Mp4Reader
}

// ReadBoxData reads the box data from an atom box.
func (b *Box) ReadBoxData() []byte {
	if b.Size <= BoxHeaderSize {
		return nil
	}
	return b.Reader.ReadBytesAt(b.Size-BoxHeaderSize, b.Start+BoxHeaderSize)
}

// readBoxes reads the child boxes of the region [start, start+n). A box with
// a corrupt header is recorded as an error and skipped; parsing then
// resynchronizes at the next plausible box header so later sibling boxes are
// still found.
func readBoxes(m *Mp4Reader, start int64, n int64) (l []*Box) {
	if n < 0 || start < 0 {
		return nil
	}
	end := start + n
	if end > m.Size {
		end = m.Size
	}
	for offset := start; offset+BoxHeaderSize <= end; {
		size, name, err := m.readBoxHeader(offset, end)
		if err != nil {
			m.addError(name, offset, err)
			next := findNextBox(m, offset+1, end)
			if next < 0 {
				break
			}
			offset = next
			continue
		}
		b := &Box{
			Name:   name,
			Size:   int64(size),
			Reader: m,
			Start:  offset,
		}
		l = append(l, b)
		offset += int64(size)
	}
	return l
}

// findNextBox scans [from, end) for the next plausible box header: a
// printable fourcc with a size that fits inside the region. It returns -1 if
// none is found.
func findNextBox(m *Mp4Reader, from, end int64) int64 {
	for p := from; p+BoxHeaderSize <= end; p++ {
		buf := m.ReadBytesAt(BoxHeaderSize, p)
		if len(buf) < BoxHeaderSize {
			return -1
		}
		size := binary.BigEndian.Uint32(buf[0:4])
		if size < BoxHeaderSize || p+int64(size) > end {
			continue
		}
		if isPrintableFourCC(buf[4:8]) {
			return p
		}
	}
	return -1
}

func isPrintableFourCC(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
