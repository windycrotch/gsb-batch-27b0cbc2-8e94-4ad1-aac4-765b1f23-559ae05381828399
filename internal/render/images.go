package render

import (
	"bytes"
	"fmt"
)

// ImagePlacement is an immutable decoded image and its visible viewport crop.
// Slots are assigned by the compositor, never by document content.
type ImagePlacement struct {
	PNG                 []byte
	Key                 int
	Rect                Placement
	X, Y, Width, Height int
}
type placedImage struct {
	Key                 int
	Rect                Placement
	X, Y, Width, Height int
}

func (p ImagePlacement) state() placedImage {
	return placedImage{p.Key, p.Rect, p.X, p.Y, p.Width, p.Height}
}

func (w *KittyWriter) SetImages(images []ImagePlacement) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.visible = false
	w.images = append([]ImagePlacement(nil), images[:min(len(images), 8)]...)
	w.schedule()
}
func (w *KittyWriter) imagesChanged() bool {
	if len(w.images) != len(w.imageStates) {
		return true
	}
	for i, p := range w.images {
		if w.imageStates[i] != p.state() {
			return true
		}
	}
	return false
}
func (w *KittyWriter) deleteImage(id int) string {
	return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
}
func (w *KittyWriter) writeImages(buf *bytes.Buffer) {
	if w.imageStates == nil {
		w.imageStates = map[int]placedImage{}
	}
	for slot, old := range w.imageStates {
		if slot >= len(w.images) || w.images[slot].Key != old.Key {
			buf.WriteString(w.deleteImage(w.imageID + slot + 1))
			delete(w.imageStates, slot)
		}
	}
	for slot, p := range w.images {
		id := w.imageID + slot + 1
		if old, ok := w.imageStates[slot]; !ok || old.Key != p.Key {
			buf.WriteString(kittyUpload(p.PNG, id))
		}
		r := p.Rect
		fmt.Fprintf(buf, "\x1b7\x1b[%d;%dH\x1b_Ga=p,i=%d,p=1,x=%d,y=%d,w=%d,h=%d,c=%d,r=%d,C=1,z=1,q=2\x1b\\\x1b8", r.Y+1, r.X+1, id, p.X, p.Y, p.Width, p.Height, r.Columns, r.Rows)
		w.imageStates[slot] = p.state()
	}
}
