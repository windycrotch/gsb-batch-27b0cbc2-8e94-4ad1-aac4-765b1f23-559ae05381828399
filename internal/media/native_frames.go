package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
)

// NativeFrames preserves video detail as PNG frames rather than terminal cells.
// Resolution, configurable sampling, and a two-frame queue bound decoder work.
func NativeFrames(ctx context.Context, path string, start float64) <-chan Frame {
	return NativeFramesAtFPS(ctx, path, start, FPS)
}

func NativeFramesAtFPS(ctx context.Context, path string, start, fps float64) <-chan Frame {
	out := make(chan Frame, 2)
	go func() {
		defer close(out)
		send := func(f Frame) bool {
			select {
			case out <- f:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if fps < 1 || fps > 60 {
			send(Frame{Err: fmt.Errorf("FPS must be between 1 and 60")})
			return
		}
		args := []string{"-v", "error", "-nostdin", "-threads", "1"}
		if start > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", start))
		}
		args = append(args, "-i", path, "-an", "-sn", "-vf", fmt.Sprintf("fps=%g,scale=w='min(iw,1920)':h='min(ih,1080)':force_original_aspect_ratio=decrease", fps), "-threads", "1", "-f", "image2pipe", "-c:v", "png", "-compression_level", "1", "pipe:1")
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			send(Frame{Err: err})
			return
		}
		if err = cmd.Start(); err != nil {
			send(Frame{Err: err})
			return
		}
		reader := bufio.NewReader(pipe)
		count := 0
		var parseErr error
		for {
			data, e := readPNG(reader)
			if e != nil {
				if e != io.EOF {
					parseErr = e
				}
				break
			}
			f := Frame{PNG: data, Time: start + float64(count)/fps, Width: int(binary.BigEndian.Uint32(data[16:20])), Height: int(binary.BigEndian.Uint32(data[20:24]))}
			if !send(f) {
				break
			}
			count++
		}
		if parseErr != nil {
			_ = cmd.Process.Kill()
		}
		err = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if parseErr != nil {
			send(Frame{Err: parseErr})
		} else if err != nil {
			send(Frame{Err: fmt.Errorf("native video decode: %w: %.200s", err, stderr.String())})
		} else if count == 0 {
			send(Frame{Err: fmt.Errorf("no video frames decoded")})
		}
	}()
	return out
}
func readPNG(r io.Reader) ([]byte, error) {
	signature := make([]byte, 8)
	if _, err := io.ReadFull(r, signature); err != nil {
		return nil, err
	}
	if !bytes.Equal(signature, []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return nil, fmt.Errorf("invalid PNG frame signature")
	}
	data := signature
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, err
		}
		n := uint64(binary.BigEndian.Uint32(header[:4]))
		if uint64(len(data))+n+12 > 20*1024*1024 {
			return nil, fmt.Errorf("PNG frame exceeds 20 MiB")
		}
		if len(data) == 8 && (string(header[4:]) != "IHDR" || n != 13) {
			return nil, fmt.Errorf("PNG frame missing IHDR")
		}
		chunk := make([]byte, int(n)+4)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		data = append(data, header...)
		data = append(data, chunk...)
		if string(header[4:]) == "IEND" {
			return data, nil
		}
	}
}
