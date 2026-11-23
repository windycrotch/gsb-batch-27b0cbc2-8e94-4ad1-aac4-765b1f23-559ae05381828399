package atom

import (
	"encoding/binary"
	"fmt"
)

// VmhdBox - Video Media Header Box
// Box Type: vmhd
// Container: Media Information Box (minf)
// Mandatory: Yes
// Quantity: Exactly one specific media header shall be present.
type VmhdBox struct {
	*Box
	Version      byte
	Flags        uint32
	GraphicsMode uint16
	OpColor      uint16
}

func (b *VmhdBox) parse() error {
	data := b.ReadBoxData()
	if len(data) < 8 {
		return fmt.Errorf("vmhd: not enough data: %d bytes", len(data))
	}
	b.Version = data[0]
	b.Flags = binary.BigEndian.Uint32(data[0:4])
	b.GraphicsMode = binary.BigEndian.Uint16(data[4:6])
	b.OpColor = binary.BigEndian.Uint16(data[6:8])
	return nil
}
