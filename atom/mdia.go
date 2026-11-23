package atom

// MdiaBox - Media Box
// Box Type: mdia
// Container: Track Box (trak)
// Mandatory: Yes
// Quantity: Exactly one.
// The media declaration container contains all the objects that declare information
// about the media data within a track.
type MdiaBox struct {
	*Box
	Hdlr *HdlrBox
	Mdhd *MdhdBox
	Minf *MinfBox
}

func (b *MdiaBox) parse() error {
	boxes := readBoxes(b.Reader, b.Start+BoxHeaderSize, b.Size-BoxHeaderSize)

	for _, box := range boxes {
		switch box.Name {
		case "hdlr":
			hdlr := &HdlrBox{Box: box}
			if b.Reader.tryParse(box, hdlr.parse) {
				b.Hdlr = hdlr
			}

		case "mdhd":
			mdhd := &MdhdBox{Box: box}
			if b.Reader.tryParse(box, mdhd.parse) {
				b.Mdhd = mdhd
			}

		case "minf":
			minf := &MinfBox{Box: box}
			if b.Reader.tryParse(box, minf.parse) {
				b.Minf = minf
			}
		}
	}
	return nil
}
