package main

import (
	"encoding/json"
	"flag"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DheerendraRathor/GoTracer/models"
	"github.com/DheerendraRathor/GoTracer/net/constants"
	"github.com/gorilla/websocket"
)

// fakeAgent is a local render agent used to exercise the master. It speaks
// the real websocket protocol and can be told to die abruptly mid-render.
type fakeAgent struct {
	cores          int
	height         int
	dieAfterPixels int // < 0: never dies

	server *httptest.Server

	mu      sync.Mutex
	patches [][2]int // column ranges it was assigned, in order
}

func newFakeAgent(t *testing.T, cores, height, dieAfterPixels int) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{cores: cores, height: height, dieAfterPixels: dieAfterPixels}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(messages.AgentStatus{Available: true, Cores: fa.cores})
	})
	mux.HandleFunc("/", fa.serveWS)

	fa.server = httptest.NewServer(mux)
	t.Cleanup(fa.server.Close)
	return fa
}

func (fa *fakeAgent) addr() string {
	return strings.TrimPrefix(fa.server.URL, "http://")
}

func (fa *fakeAgent) assignedPatches() [][2]int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	out := make([][2]int, len(fa.patches))
	copy(out, fa.patches)
	return out
}

func (fa *fakeAgent) serveWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sent := 0
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg messages.WebSocketMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Type != messages.RenderRequest {
			continue
		}

		var req messages.RenderRequestMessage
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}

		cmin, cmax := req.Data.Image.Patch[2], req.Data.Image.Patch[3]
		fa.mu.Lock()
		fa.patches = append(fa.patches, [2]int{cmin, cmax})
		fa.mu.Unlock()

		died := false
		for col := cmin; col < cmax && !died; col++ {
			for row := 0; row < fa.height; row++ {
				if fa.dieAfterPixels >= 0 && sent >= fa.dieAfterPixels {
					// Abrupt, abnormal disconnect: no close frame.
					conn.Close()
					return
				}
				err := conn.WriteJSON(messages.PixelResultMessage{
					Type:        messages.PixelResult,
					OperationId: req.OperationId,
					Data: models.Pixel{
						Color: [3]uint8{uint8(col % 256), uint8(row % 256), 200},
						I:     col,
						J:     row,
					},
				})
				if err != nil {
					return
				}
				sent++
			}
		}

		if err := conn.WriteJSON(messages.WebSocketMessage{
			Type:        messages.RenderingCompleted,
			OperationId: req.OperationId,
		}); err != nil {
			return
		}
	}
}

func testSpec(width, height int, outputFile string) models.Specification {
	var env models.Specification
	json.Unmarshal([]byte(`{
		"Settings": {"RenderRoutines": 2, "RenderDepth": 2},
		"Image": {
			"OutputFile": "`+strings.ReplaceAll(outputFile, `\`, `/`)+`",
			"Width": `+strconv.Itoa(width)+`,
			"Height": `+strconv.Itoa(height)+`,
			"Samples": 1,
			"Patch": [0, `+strconv.Itoa(height)+`, 0, `+strconv.Itoa(width)+`]
		}
	}`), &env)
	return env
}

// checkPartition verifies that ranges tile [0, width) exactly once.
func checkPartition(t *testing.T, width int, weights []int) {
	t.Helper()
	ranges := PartitionColumns(width, weights)
	if len(ranges) != len(weights) {
		t.Fatalf("expected %d ranges, got %d", len(weights), len(ranges))
	}

	covered := make([]int, width)
	prevEnd := 0
	for i, r := range ranges {
		if r[0] < 0 || r[1] > width || r[0] > r[1] {
			t.Fatalf("weights %v: range %d is invalid: %v", weights, i, r)
		}
		if r[0] != prevEnd {
			t.Fatalf("weights %v: gap or overlap before range %d: %v (previous ended at %d)", weights, i, r, prevEnd)
		}
		prevEnd = r[1]
		for col := r[0]; col < r[1]; col++ {
			covered[col]++
		}
	}
	if prevEnd != width {
		t.Fatalf("weights %v: ranges end at %d, width is %d (columns missing)", weights, prevEnd, width)
	}
	for col, n := range covered {
		if n != 1 {
			t.Fatalf("weights %v: column %d covered %d times", weights, col, n)
		}
	}
}

func TestPartitionColumns(t *testing.T) {
	cases := []struct {
		width   int
		weights []int
	}{
		{10, []int{1, 1, 1}},
		{10, []int{2, 3, 5}},
		{10, []int{0, 0, 0}},      // all agents report zero cores
		{10, []int{0, 4, 0, 6}},   // some agents report zero cores
		{3, []int{1, 1, 1, 1, 1}}, // more agents than columns
		{5, []int{1}},             // fewer agents than columns
		{1, []int{1, 1}},
		{7, []int{0}},
		{100, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{400, []int{8, 8, 8}},
		{0, []int{1, 2}},
	}

	for _, tc := range cases {
		checkPartition(t, tc.width, tc.weights)
	}

	// Exhaustive sweep: every width up to 24 against many weight patterns.
	for width := 1; width <= 24; width++ {
		for _, weights := range [][]int{
			{1, 1, 1},
			{3, 1, 2},
			{0, 1, 0},
			{0, 0},
			{5, 0, 3, 0, 2},
			{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		} {
			checkPartition(t, width, weights)
		}
	}
}

// everyPixelWritten asserts that each pixel of img has been set (alpha 255).
func everyPixelWritten(t *testing.T, img *image.RGBA) {
	t.Helper()
	bounds := img.Bounds()
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a == 0 {
				t.Fatalf("pixel (%d, %d) was never written", x, y)
			}
		}
	}
}

func connectToFakeAgents(t *testing.T, agents ...*fakeAgent) []*Agent {
	t.Helper()
	urls := make([]string, len(agents))
	for i, a := range agents {
		urls[i] = a.addr()
	}
	connected := connectAgents(urls)
	if len(connected) != len(agents) {
		t.Fatalf("expected %d connected agents, got %d", len(agents), len(connected))
	}
	t.Cleanup(func() {
		for _, a := range connected {
			a.Conn.Close()
		}
	})
	return connected
}

// TestCoordinateReassignsDisconnectedAgentWork starts several agents, kills
// one mid-render, and verifies the image is still completed exactly once.
func TestCoordinateReassignsDisconnectedAgentWork(t *testing.T) {
	width, height := 30, 12

	standby := newFakeAgent(t, 0, height, -1) // zero cores: standby
	dying := newFakeAgent(t, 2, height, 25)   // dies after 25 pixels
	healthy := newFakeAgent(t, 2, height, -1) // completes normally

	connected := connectToFakeAgents(t, standby, dying, healthy)

	env := testSpec(width, height, "")
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	stats, err := coordinate(env, connected, img)
	if err != nil {
		t.Fatalf("coordinate returned error: %v", err)
	}

	if stats.Reassignments < 1 {
		t.Fatalf("expected at least one shard reassignment, got %d", stats.Reassignments)
	}
	// Every pixel must have been written exactly once. (A reassigned shard
	// legitimately re-sends rows the dead agent had already delivered;
	// those are dropped as duplicates, never written twice.)
	if stats.PixelsWritten != width*height {
		t.Fatalf("expected %d pixels written, got %d", width*height, stats.PixelsWritten)
	}
	everyPixelWritten(t, img)

	// The union of all assigned patches must cover [0, width); completed
	// columns must not have been reassigned. Verify no two agents delivered
	// overlapping *final* coverage by checking the pixel accounting above;
	// here we additionally sanity-check that patches are within bounds.
	for _, a := range []*fakeAgent{standby, dying, healthy} {
		for _, p := range a.assignedPatches() {
			if p[0] < 0 || p[1] > width || p[0] >= p[1] {
				t.Fatalf("agent received invalid patch %v", p)
			}
		}
	}
}

// TestCoordinateMoreAgentsThanColumns ensures standby agents and zero-core
// agents do not break coverage when agents outnumber columns.
func TestCoordinateMoreAgentsThanColumns(t *testing.T) {
	width, height := 4, 6

	agents := []*fakeAgent{
		newFakeAgent(t, 1, height, -1),
		newFakeAgent(t, 0, height, -1),
		newFakeAgent(t, 1, height, -1),
		newFakeAgent(t, 1, height, -1),
		newFakeAgent(t, 1, height, -1),
	}
	connected := connectToFakeAgents(t, agents...)

	env := testSpec(width, height, "")
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	stats, err := coordinate(env, connected, img)
	if err != nil {
		t.Fatalf("coordinate returned error: %v", err)
	}
	if stats.PixelsWritten != width*height {
		t.Fatalf("expected %d pixels written, got %d", width*height, stats.PixelsWritten)
	}
	everyPixelWritten(t, img)
}

// TestCoordinateAllAgentsFail verifies the master reports failure instead of
// waiting forever or producing a partial image when every agent dies.
func TestCoordinateAllAgentsFail(t *testing.T) {
	width, height := 20, 8

	a := newFakeAgent(t, 2, height, 5)
	b := newFakeAgent(t, 2, height, 7)
	connected := connectToFakeAgents(t, a, b)

	env := testSpec(width, height, "")
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	_, err := coordinate(env, connected, img)
	if err == nil {
		t.Fatal("expected coordinate to fail when all agents disconnect, got nil error")
	}
}

// TestRunEndToEnd runs the full master workflow (spec file -> PNG file)
// with one agent dying mid-render, then verifies the decoded PNG has every
// pixel written.
func TestRunEndToEnd(t *testing.T) {
	width, height := 24, 10
	dir := t.TempDir()
	outputFile := filepath.Join(dir, "out", "render.png")

	dying := newFakeAgent(t, 2, height, 13)
	healthy := newFakeAgent(t, 2, height, -1)
	standby := newFakeAgent(t, 0, height, -1)

	env := testSpec(width, height, outputFile)
	specData, _ := json.Marshal(env)
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, specData, 0644); err != nil {
		t.Fatal(err)
	}

	agentsData, _ := json.Marshal([]string{dying.addr(), healthy.addr(), standby.addr()})
	agentsPath := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(agentsPath, agentsData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := run(specPath, agentsPath); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	decoded := decodePNG(t, outputFile)
	bounds := decoded.Bounds()
	if bounds.Dx() != width || bounds.Dy() != height {
		t.Fatalf("decoded image has bounds %v, expected %dx%d", bounds, width, height)
	}
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			if a == 0 {
				t.Fatalf("decoded PNG pixel (%d, %d) was never written", x, y)
			}
		}
	}
}

func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("unable to open output PNG: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("unable to decode output PNG: %v", err)
	}
	return img
}

// TestMasterExitsNonZeroWhenAllAgentsFail runs the real main() in a
// subprocess with agents that all die mid-render, and asserts a non-zero
// exit code and no output file.
func TestMasterExitsNonZeroWhenAllAgentsFail(t *testing.T) {
	if os.Getenv("MASTER_HELPER") == "1" {
		// Helper process: run the real master main() against the spec and
		// agents files passed via the environment.
		flag.CommandLine.Parse([]string{
			"-spec", os.Getenv("MASTER_SPEC"),
			"-agents", os.Getenv("MASTER_AGENTS"),
		})
		main()
		return
	}

	width, height := 16, 8
	dir := t.TempDir()
	outputFile := filepath.Join(dir, "should_not_exist.png")

	a := newFakeAgent(t, 2, height, 3)
	b := newFakeAgent(t, 2, height, 5)

	env := testSpec(width, height, outputFile)
	specData, _ := json.Marshal(env)
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, specData, 0644); err != nil {
		t.Fatal(err)
	}

	agentsData, _ := json.Marshal([]string{a.addr(), b.addr()})
	agentsPath := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(agentsPath, agentsData, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMasterExitsNonZeroWhenAllAgentsFail")
	cmd.Env = append(os.Environ(),
		"MASTER_HELPER=1",
		"MASTER_SPEC="+specPath,
		"MASTER_AGENTS="+agentsPath,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected master to exit non-zero, but it succeeded. Output:\n%s", output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected an exit error, got: %v", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got 0. Output:\n%s", output)
	}

	if _, statErr := os.Stat(outputFile); !os.IsNotExist(statErr) {
		t.Fatalf("partial output file %q must not exist when the render fails", outputFile)
	}
}
