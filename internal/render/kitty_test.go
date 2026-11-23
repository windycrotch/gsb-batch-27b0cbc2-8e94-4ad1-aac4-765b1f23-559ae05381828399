package render

import (
	"bytes"
	"encoding/base64"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Apply each graphics command individually, as a terminal receiving a slow
// multipart transfer would. Synchronized output is deliberately not assumed.
func TestKittyFrameHandoff(t *testing.T) {
	for _, cleanup := range []string{"help", "document", "exit", "close"} {
		t.Run(cleanup, func(t *testing.T) {
			var out bytes.Buffer
			w := &KittyWriter{out: &out, imageID: 128}
			defer w.Close()
			resident, displayed := map[int]bool{}, map[int]bool{}
			commands := regexp.MustCompile("\x1b_G([^\x1b]*)\x1b\\\\")
			consume := func(allowBlank bool) {
				t.Helper()
				pending := 0
				for _, match := range commands.FindAllStringSubmatch(out.String(), -1) {
					fields := map[string]string{}
					for _, field := range strings.Split(strings.SplitN(match[1], ";", 2)[0], ",") {
						key, value, _ := strings.Cut(field, "=")
						fields[key] = value
					}
					id, _ := strconv.Atoi(fields["i"])
					switch fields["a"] {
					case "t", "":
						if fields["a"] == "t" {
							if displayed[id] {
								t.Fatal("upload overwrites the displayed buffer")
							}
							pending = id
						}
						if fields["m"] == "0" {
							resident[pending] = true
						}
					case "p":
						if !resident[id] {
							t.Fatal("placed an incomplete image")
						}
						displayed[id] = true
					case "d":
						delete(displayed, id)
						delete(resident, id)
					}
					if !allowBlank && len(displayed) == 0 {
						t.Fatal("blank interval between video frames")
					}
				}
				out.Reset()
			}
			png := bytes.Repeat([]byte{1, 2, 3}, 5000)
			for frame := 1; frame <= 6; frame++ {
				w.Set(png, frame, Placement{Columns: 60 + frame, Rows: 20}, true)
				if _, err := w.Write([]byte("text")); err != nil {
					t.Fatal(err)
				}
				consume(frame == 1)
				if len(resident) != 1 || len(displayed) != 1 {
					t.Fatal("retired frames leaked")
				}
				for id := range resident {
					if id != 128 && id != 160 {
						t.Fatal("frame collided with document IDs")
					}
				}
			}
			switch cleanup {
			case "help":
				w.Set(nil, 7, Placement{}, false)
				w.Write([]byte("help"))
			case "document":
				w.SetImages([]ImagePlacement{{PNG: png, Key: 1, Rect: Placement{Columns: 30, Rows: 10}}})
				w.Write(nil)
			case "exit":
				w.Write([]byte("\x1b[?1049l"))
			case "close":
				w.Close()
			}
			consume(true)
			if resident[128] || resident[160] {
				t.Fatal("video frame survived cleanup")
			}
			if cleanup == "document" && !displayed[129] {
				t.Fatal("document did not replace video")
			}
		})
	}
}

func TestRendererSelection(t *testing.T) {
	for _, tt := range []struct{ request, want string }{{"auto", "cells"}, {"cells", "cells"}, {"kitty", "kitty"}} {
		got, err := Renderer(tt.request)
		if err != nil || got != tt.want {
			t.Fatalf("%s: %s %v", tt.request, got, err)
		}
	}
	if _, err := Renderer("invalid"); err == nil {
		t.Fatal("invalid renderer accepted")
	}
}
func TestKittyLifecycle(t *testing.T) {
	var out bytes.Buffer
	w := &KittyWriter{out: &out, imageID: 123}
	defer w.Close()
	png := bytes.Repeat([]byte{1, 2, 3}, 5000)
	w.Set(png, 1, Placement{X: 5, Y: 3, Columns: 60, Rows: 20}, true)
	if _, err := w.Write([]byte("text")); err != nil {
		t.Fatal(err)
	}
	if w.FrameRate() != 1 {
		t.Fatal("completed image upload not counted")
	}
	first := out.String()
	if strings.Index(first, "text") > strings.Index(first, "a=t") || !strings.Contains(first, "\x1b[4;6H") || !strings.Contains(first, "c=60,r=20,C=1") {
		t.Fatal("placement or ordering incorrect")
	}
	chunks := regexp.MustCompile("\x1b_G[^;]*;([^\x1b]*)\x1b\\\\").FindAllStringSubmatch(first, -1)
	encoded := ""
	for _, chunk := range chunks {
		if len(chunk[1]) > 4096 {
			t.Fatal("oversized chunk")
		}
		encoded += chunk[1]
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, png) {
		t.Fatal("PNG transmission changed data")
	}
	out.Reset()
	w.Write([]byte("clock"))
	if strings.Contains(out.String(), "a=t") {
		t.Fatal("unchanged image uploaded again")
	}
	if w.FrameRate() != 1 {
		t.Fatal("text-only redraw counted as a video frame")
	}
	out.Reset()
	w.Set(nil, 2, Placement{}, false)
	w.Write([]byte("help"))
	if !strings.Contains(out.String(), "\x1b_Ga=d,d=I,i=123") {
		t.Fatal("image not cleared before overlay")
	}
	out.Reset()
	w.Set(png, 3, Placement{Columns: 20, Rows: 10}, true)
	w.Write([]byte("image"))
	out.Reset()
	w.Write([]byte("\x1b[?1049l"))
	if !strings.Contains(out.String(), "a=d") || strings.Contains(out.String(), "a=p") {
		t.Fatal("image survived alt-screen exit")
	}
}

func TestKittyRefreshWithoutTextChange(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	w := &KittyWriter{out: writer, imageID: 42}
	defer w.Close()
	done := make(chan string, 1)
	go func() { buf := make([]byte, 8192); n, _ := reader.Read(buf); done <- string(buf[:n]) }()
	w.Set([]byte("frame"), 1, Placement{Columns: 10, Rows: 5}, true)
	select {
	case got := <-done:
		if !strings.Contains(got, "a=t") || !strings.Contains(got, "a=p") {
			t.Fatal("frame was not drawn")
		}
	case <-time.After(time.Second):
		t.Fatal("native frame waited for a text redraw")
	}
	// Close the pipe before cleanup so deletion cannot wait for another reader.
	reader.Close()
}
