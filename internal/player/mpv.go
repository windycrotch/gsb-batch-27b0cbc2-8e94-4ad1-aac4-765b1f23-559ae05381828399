// Package player owns an isolated mpv process and its local JSON IPC connection.
package player

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Snapshot struct {
	Position, Duration float64
	Paused, Ended      bool
	Error              string
}
type reply struct {
	Error string
	Data  json.RawMessage
}
type Player struct {
	conn    net.Conn
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	dir     string
	done    chan struct{}
	exited  chan error
	write   sync.Mutex
	mu      sync.Mutex
	state   Snapshot
	pending map[int]chan reply
	id      int
	loaded  chan error
	close   sync.Once
}

func New(ctx context.Context, audioOutput string) (*Player, error) {
	dir, err := os.MkdirTemp("", "peep-mpv-")
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	socket := filepath.Join(dir, "ipc")
	args := []string{"--no-config", "--idle=yes", "--keep-open=yes", "--pause=yes", "--vo=null", "--audio-display=no", "--terminal=no", "--input-default-bindings=no", "--input-ipc-server=" + socket, "--volume=70"}
	if audioOutput != "" {
		args = append(args, "--ao="+audioOutput)
	}
	cmd := exec.CommandContext(child, "mpv", args...)
	if err = cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("start mpv: %w", err)
	}
	p := &Player{cmd: cmd, cancel: cancel, dir: dir, done: make(chan struct{}), exited: make(chan error, 1), pending: map[int]chan reply{}}
	go func() { p.exited <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.conn, err = net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			break
		}
		select {
		case <-child.Done():
			cancel()
			<-p.exited
			os.RemoveAll(dir)
			return nil, child.Err()
		case e := <-p.exited:
			cancel()
			os.RemoveAll(dir)
			return nil, fmt.Errorf("mpv exited before IPC was ready: %v", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if p.conn == nil {
		cancel()
		<-p.exited
		os.RemoveAll(dir)
		return nil, fmt.Errorf("connect mpv: %w", err)
	}
	go p.read()
	for i, name := range []string{"time-pos", "duration", "pause", "eof-reached"} {
		if err = p.Command(ctx, "observe_property", i+1, name); err != nil {
			p.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Player) read() {
	defer close(p.done)
	scan := bufio.NewScanner(p.conn)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for scan.Scan() {
		var e struct {
			Event, Name, Error, Reason string
			RequestID                  int `json:"request_id"`
			Data                       json.RawMessage
			FileError                  string `json:"file_error"`
		}
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			continue
		}
		p.mu.Lock()
		if e.RequestID != 0 {
			if ch, ok := p.pending[e.RequestID]; ok {
				ch <- reply{Error: e.Error, Data: e.Data}
				delete(p.pending, e.RequestID)
			}
		}
		switch e.Event {
		case "property-change":
			switch e.Name {
			case "time-pos":
				_ = json.Unmarshal(e.Data, &p.state.Position)
			case "duration":
				_ = json.Unmarshal(e.Data, &p.state.Duration)
			case "pause":
				_ = json.Unmarshal(e.Data, &p.state.Paused)
			case "eof-reached":
				_ = json.Unmarshal(e.Data, &p.state.Ended)
			}
		case "file-loaded":
			if p.loaded != nil {
				select {
				case p.loaded <- nil:
				default:
				}
			}
		case "end-file":
			if e.Reason == "error" {
				p.state.Error = "mpv: " + e.FileError
				if p.loaded != nil {
					select {
					case p.loaded <- errors.New(p.state.Error):
					default:
					}
				}
			}
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.state.Error = "mpv connection closed"
	p.mu.Unlock()
}

func (p *Player) Command(ctx context.Context, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p.mu.Lock()
	p.id++
	id := p.id
	ch := make(chan reply, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	data, err := json.Marshal(map[string]any{"command": args, "request_id": id})
	if err != nil {
		return err
	}
	p.write.Lock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = p.conn.Write(append(data, '\n'))
	p.write.Unlock()
	if err != nil {
		return fmt.Errorf("write mpv command: %w", err)
	}
	select {
	case r := <-ch:
		if r.Error != "success" {
			return fmt.Errorf("mpv %v: %s", args[0], r.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("mpv disconnected")
	}
}

// Load must be called serially; callers cancel a pending load before replacing it.
func (p *Player) Load(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	p.mu.Lock()
	p.loaded = ready
	p.state = Snapshot{Paused: true}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.loaded == ready {
			p.loaded = nil
		}
		p.mu.Unlock()
	}()
	if err := p.Command(ctx, "set_property", "pause", true); err != nil {
		return err
	}
	if err := p.Command(ctx, "loadfile", path, "replace"); err != nil {
		return err
	}
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("mpv disconnected")
	}
}
func (p *Player) Snapshot() Snapshot { p.mu.Lock(); defer p.mu.Unlock(); return p.state }
func (p *Player) Pause(ctx context.Context, pause bool) error {
	return p.Command(ctx, "set_property", "pause", pause)
}
func (p *Player) Seek(ctx context.Context, pos float64) error {
	if err := p.Command(ctx, "seek", pos, "absolute+exact"); err != nil {
		return err
	}
	// The seek reply means mpv accepted the target. Publish it before the next
	// property push arrives so Snapshot never reports the pre-seek position;
	// consumers would otherwise drop or hold frames from the fresh decoder.
	p.mu.Lock()
	p.state.Position = pos
	p.state.Ended = false
	p.mu.Unlock()
	return nil
}
func (p *Player) Close() {
	p.close.Do(func() { p.cancel(); _ = p.conn.Close(); <-p.exited; <-p.done; _ = os.RemoveAll(p.dir) })
}
