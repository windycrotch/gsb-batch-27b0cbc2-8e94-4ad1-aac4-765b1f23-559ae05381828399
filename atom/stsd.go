package atom

import (
	"encoding/binary"
	"fmt"
)

// StsdBox - Sample Description Box
// Box Type: stsd
// Container: Sample Table Box (stbl)
// Mandatory: Yes
// Quantity: Exactly one.
type StsdBox struct {
	*Box
	Version byte
	Flags   uint32
	Avc1    *Avc1Box
}

func (b *StsdBox) parse() error {
	data := b.ReadBoxData()
	if len(data) < 8 {
		return fmt.Errorf("stsd: not enough data: %d bytes", len(data))
	}
	b.Version = data[0]
	b.Flags = binary.BigEndian.Uint32(data[0:4])

	// Skip the 8-byte version/flags + entry-count prefix; entries are boxes.
	boxes := readBoxes(b.Reader, b.Start+BoxHeaderSize+8, b.Size-BoxHeaderSize-8)

	for _, box := range boxes {
		switch box.Name {
		case "avc1":
			avc1 := &Avc1Box{Box: box}
			if b.Reader.tryParse(box, avc1.parse) {
				b.Avc1 = avc1
			}
		}
	}
	return nil
}
