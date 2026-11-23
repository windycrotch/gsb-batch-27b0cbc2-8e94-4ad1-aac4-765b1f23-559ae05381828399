package atom

// StblBox - Sample Table Box
// Box Type: stbl
// Container: Media Information Box (minf)
// Mandatory: Yes
// Quantity: Exactly one.
type StblBox struct {
	*Box
	Stts *SttsBox
	Stsd *StsdBox
}

func (b *StblBox) parse() error {
	boxes := readBoxes(b.Reader, b.Start+BoxHeaderSize, b.Size-BoxHeaderSize)

	for _, box := range boxes {
		switch box.Name {
		case "stts":
			stts := &SttsBox{Box: box}
			if b.Reader.tryParse(box, stts.parse) {
				b.Stts = stts
			}

		case "stsd":
			stsd := &StsdBox{Box: box}
			if b.Reader.tryParse(box, stsd.parse) {
				b.Stsd = stsd
			}
		}
	}
	return nil
}
