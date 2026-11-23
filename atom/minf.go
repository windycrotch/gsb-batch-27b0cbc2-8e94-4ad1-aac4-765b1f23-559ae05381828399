package atom

// MinfBox - Media Information Box
// Box Type: minf
// Container: Media Box (mdia)
// Mandatory: Yes
// Quantity: Exactly one.
//
// This box contains all the objects that declare characteristics information of the
// media in the track.
type MinfBox struct {
	*Box
	Vmhd *VmhdBox
	// Dinf *DinfBox
	Stbl *StblBox
	Hmhd *HmhdBox
}

func (b *MinfBox) parse() error {
	boxes := readBoxes(b.Reader, b.Start+BoxHeaderSize, b.Size-BoxHeaderSize)

	for _, box := range boxes {
		switch box.Name {
		case "vmhd":
			vmhd := &VmhdBox{Box: box}
			if b.Reader.tryParse(box, vmhd.parse) {
				b.Vmhd = vmhd
			}

		case "stbl":
			stbl := &StblBox{Box: box}
			if b.Reader.tryParse(box, stbl.parse) {
				b.Stbl = stbl
			}

		case "hmhd":
			hmhd := &HmhdBox{Box: box}
			if b.Reader.tryParse(box, hmhd.parse) {
				b.Hmhd = hmhd
			}
		}
	}
	return nil
}
