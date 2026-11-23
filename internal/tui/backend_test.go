package tui

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

	"github.com/lancekrogers/peep/internal/player"
)

// This file drives the model with a real player.Player connected to a fake
// mpv: a copy of the test binary placed on PATH. The child recognizes
// PEEP_FAKE_MPV in init and speaks just enough JSON IPC, so no real mpv,
// terminal, or network is needed.
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
		if name == "seek" && len(cmd.Command) >= 2 {
			if pos, ok := cmd.Command[1].(float64); ok {
				// Delay the property push: only a client that trusts the seek
				// reply keeps a consistent clock in the meantime.
				go func() {
					time.Sleep(300 * time.Millisecond)
					send(map[string]any{"event": "property-change", "name": "time-pos", "data": pos})
				}()
			}
		}
	}
	return 0
}

func installFakeMPV(t *testing.T) {
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
}

func TestSeekKeepsClockAndDecoderConsistent(t *testing.T) {
	installFakeMPV(t)
	f := installFakeDecoder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend, err := player.New(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m := videoModel()
	m.ctx = ctx
	m.backend = backend
	m.state = "Playing"
	m.position = 5
	m.showActive()
	defer backend.Close()

	for _, target := range []float64{20, 4} {
		cmd := m.seek(target)
		if cmd == nil {
			t.Fatal("seek command rejected")
		}
		m.Update(cmd()) // deliver controlMsg with the seek result
		if m.position != target {
			t.Fatalf("seek target %v not applied: %v", target, m.position)
		}
		// A tick syncing against the player must not rewind the clock to the
		// pre-seek position while the property push is still in flight.
		m.Update(tickMsg(time.Now()))
		if m.position != target {
			t.Fatalf("clock regressed after seek to %v: %v", target, m.position)
		}
	}
	if f.count() != 3 || f.startAt(1) != 20 || f.startAt(2) != 4 {
		t.Fatalf("decoder did not follow seeks: %v", f.starts)
	}
	if !f.cancelled(0) || !f.cancelled(1) {
		t.Fatal("replaced decoders were not cancelled")
	}
}
