package atom

import "fmt"

// Avc1Box defines the avc1 box structure.
type Avc1Box struct {
	*Box
	Version byte
}

func (b *Avc1Box) parse() error {
	data := b.ReadBoxData()
	if len(data) < 1 {
		return fmt.Errorf("avc1: not enough data: %d bytes", len(data))
	}
	b.Version = data[0]
	return nil
}
