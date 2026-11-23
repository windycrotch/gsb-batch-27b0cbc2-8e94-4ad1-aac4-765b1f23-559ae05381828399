package render

import (
	"bytes"
	"strings"
	"testing"
)

func TestDocumentImageLifecycle(t *testing.T) {
	var out bytes.Buffer
	w := &KittyWriter{out: &out, imageID: 100}
	defer w.Close()
	images := []ImagePlacement{
		{PNG: []byte("first"), Key: 1, Rect: Placement{X: 5, Y: 3, Columns: 20, Rows: 5}, Width: 200, Height: 100},
		{PNG: []byte("second"), Key: 2, Rect: Placement{X: 5, Y: 9, Columns: 20, Rows: 4}, Y: 20, Width: 200, Height: 80},
	}
	w.SetImages(images)
	w.Write([]byte("text"))
	if strings.Count(out.String(), "a=t") != 2 || !strings.Contains(out.String(), "i=102,p=1,x=0,y=20,w=200,h=80") {
		t.Fatal("missing images/crop", out.String())
	}
	out.Reset()
	images[0].Y = 10
	images[0].Height = 90
	w.SetImages(images)
	w.Write(nil)
	if strings.Contains(out.String(), "a=t") || !strings.Contains(out.String(), "y=10,w=200,h=90") {
		t.Fatal("scroll reuploaded image or lost crop")
	}
	out.Reset()
	w.SetImages(nil)
	w.Write([]byte("help"))
	if strings.Count(out.String(), "a=d") != 2 || strings.Contains(out.String(), "a=p") {
		t.Fatal("images survived overlay")
	}
	w.SetImages(images)
	w.Write(nil)
	out.Reset()
	w.Set([]byte("video"), 3, Placement{Columns: 20, Rows: 10}, true)
	w.Write(nil)
	if strings.Count(out.String(), "a=d") != 2 || !strings.Contains(out.String(), "i=100") {
		t.Fatal("media transition retained document images")
	}
	w.SetImages(images)
	w.Write(nil)
	out.Reset()
	w.Write([]byte("\x1b[?1049l"))
	if strings.Count(out.String(), "a=d") != 2 || strings.Contains(out.String(), "a=p") {
		t.Fatal("images survived exit")
	}
}
