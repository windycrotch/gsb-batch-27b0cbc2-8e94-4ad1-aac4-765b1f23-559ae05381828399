package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// PreviewPNG decodes a static poster without the terminal-cell resolution cap.
// The maximum dimension is 2048 pixels; small originals are not upscaled.
func PreviewPNG(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-threads", "1", "-i", path, "-an", "-sn", "-vf", "scale=w='min(iw,2048)':h='min(ih,2048)':force_original_aspect_ratio=decrease", "-frames:v", "1", "-threads", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	output := &previewBuffer{}
	var stderr bytes.Buffer
	cmd.Stdout = output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("decode image preview: %w: %.200s", err, stderr.String())
	}
	if output.Len() == 0 {
		return nil, fmt.Errorf("image preview produced no pixels")
	}
	return output.Bytes(), nil
}

type previewBuffer struct{ bytes.Buffer }

func (b *previewBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 20*1024*1024 {
		return 0, fmt.Errorf("image preview exceeds 20 MiB")
	}
	return b.Buffer.Write(p)
}
