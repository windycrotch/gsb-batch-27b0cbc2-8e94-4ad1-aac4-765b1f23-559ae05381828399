package render

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ProbeInput preserves the terminal file interface and removes only replies to
// our graphics query. Ordinary keys continue through Bubble Tea during probing.
type ProbeInput struct {
	*os.File
	id     int
	result chan bool
}

func NewProbeInput(file *os.File) *ProbeInput {
	return &ProbeInput{File: file, id: os.Getpid() + 100000, result: make(chan bool, 1)}
}

// Detect queries without creating an image. A missing or negative reply selects
// cells. Explicit renderer overrides and multiplexers never call this method.
func (p *ProbeInput) Detect(ctx context.Context, out io.Writer) bool {
	query := fmt.Sprintf("\x1b_Gi=%d,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\", p.id)
	if _, err := io.WriteString(out, query); err != nil {
		return false
	}
	timer := time.NewTimer(350 * time.Millisecond)
	defer timer.Stop()
	select {
	case supported := <-p.result:
		return supported
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (p *ProbeInput) Read(dst []byte) (int, error) {
	n, err := p.File.Read(dst)
	if n == 0 {
		return n, err
	}
	data := append([]byte(nil), dst[:n]...)
	// A terminal reply may be fragmented. Wait briefly for an incomplete APC,
	// bounded in both time and bytes. An ordinary Escape key is delayed at most
	// 20 ms, and unrelated input is never discarded.
	deadline := time.Now().Add(100 * time.Millisecond)
	for incompleteAPC(data) && len(data) < len(dst) && time.Now().Before(deadline) {
		fds := []unix.PollFd{{Fd: int32(p.Fd()), Events: unix.POLLIN}}
		ready, pollErr := unix.Poll(fds, 20)
		if pollErr != nil || ready == 0 || fds[0].Revents&unix.POLLIN == 0 {
			break
		}
		extra := make([]byte, len(dst)-len(data))
		count, readErr := p.File.Read(extra)
		data = append(data, extra[:count]...)
		if readErr != nil {
			err = readErr
			break
		}
	}
	clean, found, supported := stripProbeReply(data, p.id)
	if found {
		select {
		case p.result <- supported:
		default:
		}
	}
	return copy(dst, clean), err
}

func incompleteAPC(data []byte) bool {
	if bytes.HasSuffix(data, []byte{0x1b}) {
		return true
	}
	start := bytes.LastIndex(data, []byte("\x1b_"))
	return start >= 0 && !bytes.Contains(data[start:], []byte("\x1b\\"))
}

func stripProbeReply(data []byte, id int) ([]byte, bool, bool) {
	var clean []byte
	found, supported := false, false
	for len(data) > 0 {
		start := bytes.Index(data, []byte("\x1b_G"))
		if start < 0 {
			clean = append(clean, data...)
			break
		}
		end := bytes.Index(data[start:], []byte("\x1b\\"))
		if end < 0 {
			clean = append(clean, data...)
			break
		}
		end += start + 2
		header, payload, ok := strings.Cut(string(data[start+3:end-2]), ";")
		matches := false
		for _, field := range strings.Split(header, ",") {
			if field == "i="+strconv.Itoa(id) {
				matches = true
			}
		}
		clean = append(clean, data[:start]...)
		if ok && matches {
			found = true
			supported = payload == "OK"
		} else {
			clean = append(clean, data[start:end]...)
		}
		data = data[end:]
	}
	return clean, found, supported
}
