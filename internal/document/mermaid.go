package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mermaid runs a private rendering worker in the peep executable. Its bounded
// cache belongs to the application instance, not to a document or the filesystem.
type Mermaid struct {
	executable string
	mu         sync.Mutex
	cache      map[[32]byte][]byte
	order      [][32]byte
	bytes      int
}

// Resolve once, before an install/update can switch the public executable link.
var selfExecutable = func() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}()

func NewMermaid(executable string) *Mermaid {
	if executable == "" {
		executable = selfExecutable
	}
	return &Mermaid{executable: executable, cache: map[[32]byte][]byte{}}
}

type limitedOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("renderer output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (r *Mermaid) Render(ctx context.Context, source string, columns int) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if len(source) > 128<<10 {
		return Image{}, fmt.Errorf("Mermaid source exceeds 128 KiB; showing source")
	}
	executable, err := exec.LookPath(r.executable)
	if err != nil {
		return Image{}, fmt.Errorf("peep rendering worker unavailable: %w; reinstall peep", err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		return Image{}, fmt.Errorf("Mermaid helper: %w", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d:%d\x00%s\x00dark:1600\x00%s", executable, info.ModTime().UnixNano(), info.Size(), time.Now().UTC().Format("2006-01-02"), source)))
	r.mu.Lock()
	data := r.cache[key]
	r.mu.Unlock()
	if data != nil {
		return ImageFromPNG(data, columns)
	}
	child, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(child, executable, "__peep_mermaid_png_v1")
	cmd.Stdin = strings.NewReader(source)
	out := &limitedOutput{limit: 20 << 20}
	stderr := &limitedOutput{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if child.Err() != nil {
			return Image{}, fmt.Errorf("Mermaid render: %w", child.Err())
		}
		return Image{}, fmt.Errorf("Mermaid render failed: %w: %.300s", err, strings.TrimSpace(stderr.String()))
	}
	image, err := ImageFromPNG(out.Bytes(), columns)
	if err != nil {
		return Image{}, err
	}
	r.mu.Lock()
	if _, exists := r.cache[key]; !exists {
		for len(r.order) > 0 && (len(r.order) >= 16 || r.bytes+len(image.PNG) > 32<<20) {
			old := r.order[0]
			r.order = r.order[1:]
			r.bytes -= len(r.cache[old])
			delete(r.cache, old)
		}
		r.cache[key] = image.PNG
		r.order = append(r.order, key)
		r.bytes += len(image.PNG)
	}
	r.mu.Unlock()
	return image, nil
}
