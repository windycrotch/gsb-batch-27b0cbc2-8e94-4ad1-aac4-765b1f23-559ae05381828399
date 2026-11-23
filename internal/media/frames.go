package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const FPS = 12.0

type Frame struct {
	PNG           []byte
	RGB           []byte
	Width, Height int
	Time          float64
	Err           error
}

// Frames uses a two-frame queue. Cancellation kills the decoder and unblocks readers.
// ffmpeg scales with square pixels; two vertical pixels become one terminal cell.
func Frames(ctx context.Context, path string, width, height int, start float64, single bool) <-chan Frame {
	return FramesAtFPS(ctx, path, width, height, start, single, FPS)
}

func FramesAtFPS(ctx context.Context, path string, width, height int, start float64, single bool, fps float64) <-chan Frame {
	out := make(chan Frame, 2)
	go func() {
		defer close(out)
		decodeCtx := ctx
		if single {
			var cancel context.CancelFunc
			decodeCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
		}
		if single && (Classify(path) == Image || Classify(path) == Audio) {
			start = 0
		}
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
		if width < 1 || height < 2 || width > 240 || height > 160 {
			send(Frame{Err: fmt.Errorf("invalid frame size %dx%d", width, height)})
			return
		}
		filter := fmt.Sprintf("format=rgb24,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1", width, height, width, height)
		if !single {
			filter = fmt.Sprintf("fps=%g,", fps) + filter
		}
		args := []string{"-v", "error", "-nostdin", "-threads", "1"}
		// Seeking even to zero can discard the only JPEG packet. Stills never seek.
		if start > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", start))
		}
		args = append(args, "-i", path, "-an", "-sn", "-vf", filter, "-threads", "1")
		if single {
			args = append(args, "-frames:v", "1")
		}
		args = append(args, "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
		cmd := exec.CommandContext(decodeCtx, "ffmpeg", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			send(Frame{Err: err})
			return
		}
		if err = cmd.Start(); err != nil {
			send(Frame{Err: fmt.Errorf("start ffmpeg: %w", err)})
			return
		}
		count := 0
		for {
			buf := make([]byte, width*height*3)
			_, err = io.ReadFull(pipe, buf)
			if err != nil {
				break
			}
			if !send(Frame{RGB: buf, Width: width, Height: height, Time: start + float64(count)/fps}) {
				break
			}
			count++
		}
		err = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if decodeCtx.Err() != nil {
			send(Frame{Err: fmt.Errorf("preview timed out: %w", decodeCtx.Err())})
			return
		}
		if err != nil {
			send(Frame{Err: fmt.Errorf("decode %s: %w: %.300s", path, err, stderr.String())})
		} else if count == 0 {
			send(Frame{Err: fmt.Errorf("no visual frame in %s", path)})
		}
	}()
	return out
}
