package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Renderer validates the override. Auto starts with cells until a terminal
// capability probe succeeds; no environment variable implies graphics support.
func Renderer(request string) (string, error) {
	if request == "cells" || request == "kitty" {
		return request, nil
	}
	if request != "auto" {
		return "", fmt.Errorf("unknown renderer %q; use auto, kitty, or cells", request)
	}
	return "cells", nil
}

type Placement struct{ X, Y, Columns, Rows int }

// KittyWriter is the only terminal writer. It serializes Bubble Tea text and
// image commands, places images after text, and removes owned images on exit.
// Fd preserves Bubble Tea's TTY-size detection when using this output wrapper.
type KittyWriter struct {
	frameTimes                []time.Time
	images                    []ImagePlacement
	imageStates               map[int]placedImage
	timer                     *time.Timer
	drawn                     Placement
	out                       io.Writer
	fd                        uintptr
	mu                        sync.Mutex
	png                       []byte
	key, sent                 int
	rect                      Placement
	visible, uploaded, closed bool
	imageID                   int
	frameSlot                 int
}

func NewKittyWriter(out *os.File) *KittyWriter {
	return &KittyWriter{out: out, fd: out.Fd(), imageID: os.Getpid() * 64}
}
func (w *KittyWriter) Fd() uintptr { return w.fd }
func (w *KittyWriter) Set(png []byte, key int, rect Placement, visible bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.images = nil
	w.png = png
	w.key = key
	w.rect = rect
	w.visible = visible && len(png) > 0 && rect.Columns > 0 && rect.Rows > 0
	w.schedule()
}
func (w *KittyWriter) schedule() {
	if !w.closed && w.timer == nil && w.needsDraw() {
		w.timer = time.AfterFunc(time.Second/60, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.timer = nil
			if !w.closed && w.needsDraw() {
				_, _ = w.writeLocked(nil)
			}
		})
	}
}

// Document images use offsets 1..8. Reserve offset 32 for the second video
// buffer, within this writer's 64-ID namespace.
func (w *KittyWriter) frameID() int   { return w.imageID + w.frameSlot*32 }
func (w *KittyWriter) remove() string { return w.deleteImage(w.frameID()) }
func (w *KittyWriter) needsDraw() bool {
	return w.imagesChanged() || (w.uploaded && (!w.visible || w.sent != w.key || w.drawn != w.rect)) || (w.visible && (!w.uploaded || w.sent != w.key || w.drawn != w.rect))
}
func (w *KittyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(p)
}
func (w *KittyWriter) writeLocked(p []byte) (int, error) {
	if bytes.Contains(p, []byte("\x1b[?1049h")) {
		w.closed = false
	}
	exiting := bytes.Contains(p, []byte("\x1b[?1049l"))
	if exiting {
		w.visible = false
		w.images = nil
		w.closed = true
	}
	uploadedFrame := false
	var buf bytes.Buffer
	buf.WriteString("\x1b[?2026h")
	if w.uploaded && !w.visible {
		buf.WriteString(w.remove())
		w.uploaded = false
	}
	buf.Write(p)
	if w.visible && !w.closed {
		previousID := 0
		if !w.uploaded || w.sent != w.key {
			if w.uploaded {
				previousID = w.frameID()
				w.frameSlot = 1 - w.frameSlot
			}
			// Keep the previous placement alive throughout the upload. Deleting
			// it first exposes a blank frame in terminals such as iTerm, even
			// when the transfer is wrapped in synchronized output.
			buf.WriteString(kittyUpload(w.png, w.frameID()))
			w.uploaded = true
			w.sent = w.key
			uploadedFrame = true
		}
		r := w.rect
		w.drawn = r
		// Save/restore cursor; C=1 prevents placement from advancing or scrolling it.
		fmt.Fprintf(&buf, "\x1b7\x1b[%d;%dH\x1b_Ga=p,i=%d,p=1,c=%d,r=%d,C=1,z=1,q=2\x1b\\\x1b8", r.Y+1, r.X+1, w.frameID(), r.Columns, r.Rows)
		if previousID != 0 {
			buf.WriteString(w.deleteImage(previousID))
		}
	}
	w.writeImages(&buf)
	buf.WriteString("\x1b[?2026l")
	if _, err := w.out.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	if uploadedFrame {
		now := time.Now()
		w.frameTimes = append(w.frameTimes, now)
		first := 0
		for first < len(w.frameTimes) && now.Sub(w.frameTimes[first]) >= time.Second {
			first++
		}
		w.frameTimes = w.frameTimes[first:]
	}
	return len(p), nil
}
func kittyUpload(png []byte, id int) string {
	data := base64.StdEncoding.EncodeToString(png)
	var b strings.Builder
	for offset := 0; offset < len(data); offset += 4096 {
		end := min(offset+4096, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&b, "\x1b_Ga=t,f=100,t=d,i=%d,q=2,m=%d;", id, more)
		} else {
			// iTerm evaluates reply verbosity on the final chunk rather than
			// inheriting it from the first. Keep every chunk quiet so its ACK
			// cannot reach the TUI as keystrokes and restart the preview.
			fmt.Fprintf(&b, "\x1b_Gm=%d,q=2;", more)
		}
		b.WriteString(data[offset:end])
		b.WriteString("\x1b\\")
	}
	return b.String()
}

func (w *KittyWriter) Read(p []byte) (int, error) {
	if r, ok := w.out.(io.Reader); ok {
		return r.Read(p)
	}
	return 0, io.EOF
}
func (w *KittyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	var buf bytes.Buffer
	for slot := range w.imageStates {
		buf.WriteString(w.deleteImage(w.imageID + slot + 1))
	}
	w.imageStates = nil
	w.images = nil
	if w.uploaded {
		w.uploaded = false
		buf.WriteString(w.remove())
	}
	if buf.Len() > 0 {
		_, err := w.out.Write(buf.Bytes())
		return err
	}
	return nil
}

// FrameRate counts complete new video frames sent to the terminal in the last
// second. It cannot measure the terminal emulator's physical refresh rate.
func (w *KittyWriter) FrameRate() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := 0
	for _, at := range w.frameTimes {
		if time.Since(at) < time.Second {
			count++
		}
	}
	return count
}
