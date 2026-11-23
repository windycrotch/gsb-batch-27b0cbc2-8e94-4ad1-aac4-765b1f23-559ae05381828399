package player

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file run a fake mpv: a copy of the test binary placed on
// PATH. The child recognizes PEEP_FAKE_MPV in init and speaks just enough of
// the JSON IPC protocol over a local socket, so no real mpv is needed.
func init() {
	if os.Getenv("PEEP_FAKE_MPV") != "" {
		os.Exit(fakeMPV())
	}
}

func fakeMPV() int {
	socket := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--input-ipc-server=") {
			socket = strings.TrimPrefix(arg, "--input-ipc-server=")
		}
	}
	if socket == "" {
		fmt.Fprintln(os.Stderr, "fake mpv: no IPC server path")
		return 2
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake mpv listen:", err)
		return 1
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		return 1
	}
	defer conn.Close()
	var writeMu sync.Mutex
	send := func(v any) {
		data, _ := json.Marshal(v)
		writeMu.Lock()
		conn.Write(append(data, '\n'))
		writeMu.Unlock()
	}
	loadFails := os.Getenv("PEEP_FAKE_MPV_LOAD_ERROR") != ""
	scan := bufio.NewScanner(conn)
	for scan.Scan() {
		var cmd struct {
			RequestID int   `json:"request_id"`
			Command   []any `json:"command"`
		}
		if json.Unmarshal(scan.Bytes(), &cmd) != nil || cmd.RequestID == 0 || len(cmd.Command) == 0 {
			continue
		}
		name, _ := cmd.Command[0].(string)
		send(map[string]any{"request_id": cmd.RequestID, "error": "success"})
		switch name {
		case "set_property":
			if len(cmd.Command) == 3 && cmd.Command[1] == "pause" {
				send(map[string]any{"event": "property-change", "name": "pause", "data": cmd.Command[2]})
			}
		case "loadfile":
			if loadFails {
				send(map[string]any{"event": "end-file", "reason": "error", "file_error": "open failed"})
				continue
			}
			send(map[string]any{"event": "file-loaded"})
			send(map[string]any{"event": "property-change", "name": "time-pos", "data": 0.0})
			send(map[string]any{"event": "property-change", "name": "duration", "data": 100.0})
		case "seek":
			// Delay the property push: the client must trust the seek reply,
			// not wait for the event, to keep its clock consistent.
			if len(cmd.Command) >= 2 {
				if pos, ok := cmd.Command[1].(float64); ok {
					go func() {
						time.Sleep(300 * time.Millisecond)
						send(map[string]any{"event": "property-change", "name": "time-pos", "data": pos})
					}()
				}
			}
		}
	}
	return 0
}

// installFakeMPV puts the fake player on PATH.
func installFakeMPV(t *testing.T, loadError bool) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "mpv"
	if runtime.GOOS == "windows" {
		name = "mpv.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PEEP_FAKE_MPV", "1")
	if loadError {
		t.Setenv("PEEP_FAKE_MPV_LOAD_ERROR", "1")
	}
}

func TestPlayerLifecycleWithFakeMpv(t *testing.T) {
	installFakeMPV(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := New(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Load(ctx, "video.mp4"); err != nil {
		t.Fatal(err)
	}
	if snap := p.Snapshot(); !snap.Paused || snap.Position != 0 {
		t.Fatalf("fresh load must start paused at zero: %+v", snap)
	}
	if err := p.Pause(ctx, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !p.Snapshot().Paused })
	if err := p.Seek(ctx, 12.5); err != nil {
		t.Fatal(err)
	}
	// The seek reply alone must move the reported clock; the property event
	// arrives later and must not rewind it.
	if pos := p.Snapshot().Position; pos != 12.5 {
		t.Fatalf("clock did not follow the seek: %v", pos)
	}
	eventually(t, func() bool { return p.Snapshot().Position == 12.5 })
	if err := p.Seek(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if pos := p.Snapshot().Position; pos != 3 {
		t.Fatalf("backward seek did not move the clock: %v", pos)
	}
	dir := p.dir
	p.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("IPC directory not removed on close")
	}
}

func TestPlayerLoadErrorSurfaces(t *testing.T) {
	installFakeMPV(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := New(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, "missing.mp4")
	if err == nil || !strings.Contains(err.Error(), "open failed") {
		t.Fatalf("load error not reported: %v", err)
	}
	if snap := p.Snapshot(); snap.Error == "" {
		t.Fatal("player state did not record the load error")
	}
	dir := p.dir
	p.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("IPC directory not removed on close")
	}
}
