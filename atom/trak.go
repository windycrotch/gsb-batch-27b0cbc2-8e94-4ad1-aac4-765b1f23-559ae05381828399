package atom

// TrakBox - Track Box
// Box Type: tkhd
// Container: Movie Box (moov)
// Mandatory: Yes
// Quantity: One or more.
type TrakBox struct {
	*Box
	// SamplesDuration
	// SamplesSize
	// SampleGroupsInfo

	Tkhd *TkhdBox
	Mdia *MdiaBox
	Edts *EdtsBox
}

func (b *TrakBox) parse() error {
	boxes := readBoxes(b.Reader, b.Start+BoxHeaderSize, b.Size-BoxHeaderSize)

	for _, box := range boxes {
		switch box.Name {
		case "tkhd":
			tkhd := &TkhdBox{Box: box}
			if b.Reader.tryParse(box, tkhd.parse) {
				b.Tkhd = tkhd
			}

		case "mdia":
			mdia := &MdiaBox{Box: box}
			if b.Reader.tryParse(box, mdia.parse) {
				b.Mdia = mdia
			}

		case "edts":
			edts := &EdtsBox{Box: box}
			if b.Reader.tryParse(box, edts.parse) {
				b.Edts = edts
			}
		}
	}
	return nil
}
