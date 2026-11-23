package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/DheerendraRathor/GoTracer/models"
	"github.com/DheerendraRathor/GoTracer/net/constants"
	"github.com/DheerendraRathor/GoTracer/utils"
	"github.com/gorilla/websocket"
)

const (
	pongTimeout = 60 * time.Second
)

var renderSpecFile string
var agentsFile string

func init() {
	flag.StringVar(&renderSpecFile, "spec", "sample_world.json", "Name of JSON file containing rendering spec")
	flag.StringVar(&agentsFile, "agents", "agents.json", "Name of JSON file containing list of agents")
}

// Agent is a connected render agent.
type Agent struct {
	URL   string
	Cores int
	Conn  *websocket.Conn
}

// shard is a half-open range of image columns [Min, Max) assigned to an agent.
type shard struct {
	ID  string
	Min int
	Max int
}

// PartitionColumns splits the column range [0, width) into len(weights)
// contiguous, non-overlapping ranges, sized proportionally to the weights
// (agent core counts). Every column is covered exactly once: no gaps and
// no overlaps, even when there are more agents than columns or some agents
// report zero cores (such agents simply get an empty range). If all weights
// are zero the columns are distributed evenly.
func PartitionColumns(width int, weights []int) [][2]int {
	n := len(weights)
	ranges := make([][2]int, n)
	if width <= 0 || n == 0 {
		return ranges
	}

	ws := make([]int, n)
	total := 0
	for i, w := range weights {
		if w > 0 {
			ws[i] = w
		}
		total += ws[i]
	}
	if total == 0 {
		for i := range ws {
			ws[i] = 1
		}
		total = n
	}

	// Cumulative proportional split: range i covers
	// [width*cum(i)/total, width*cum(i+1)/total). Because both bounds come
	// from the same monotonic cumulative sequence, the ranges tile
	// [0, width) exactly with no gaps or overlaps.
	start := 0
	cum := 0
	for i, w := range ws {
		cum += w
		end := int(int64(width) * int64(cum) / int64(total))
		if end < start {
			end = start
		}
		if end > width {
			end = width
		}
		ranges[i] = [2]int{start, end}
		start = end
	}
	return ranges
}

// eventKind distinguishes a normally completed agent from an abnormal
// disconnect, plus pixel and rejection events.
type eventKind int

const (
	evPixel eventKind = iota
	evCompleted
	evDisconnected
	evRejected
)

type agentEvent struct {
	agent *Agent
	kind  eventKind
	pixel models.Pixel
}

type agentState struct {
	agent *Agent
	alive bool
	busy  bool
	shard shard
}

// Stats summarizes a finished (or failed) distributed render.
type Stats struct {
	PixelsWritten     int
	DuplicatesDropped int
	Reassignments     int
}

type coordinator struct {
	env    models.Specification
	img    *image.RGBA
	width  int
	height int

	// colRemaining[x] = number of rows not yet received for column x.
	// received[x][y] tracks exactly which pixels arrived, so a reassigned
	// shard re-sending already-received rows is deduplicated per pixel and
	// no row is ever left unwritten.
	colRemaining []int
	received     [][]bool
	columnsLeft  int

	pending    []shard
	states     []*agentState
	liveAgents int
	events     chan agentEvent
	progress   *utils.Progress
	stats      Stats
	shardSeq   int
}

func (c *coordinator) newShard(min, max int) shard {
	c.shardSeq++
	return shard{ID: fmt.Sprintf("shard-%d-%d-%d", min, max, c.shardSeq), Min: min, Max: max}
}

// readPump runs per agent for the lifetime of its connection. It forwards
// pixels, completion notices and rejections to the event loop, and reports
// an abnormal disconnect when the read fails.
func (c *coordinator) readPump(a *Agent) {
	for {
		_, raw, err := a.Conn.ReadMessage()
		if err != nil {
			c.events <- agentEvent{agent: a, kind: evDisconnected}
			return
		}

		var message messages.WebSocketMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}

		switch message.Type {
		case messages.PixelResult:
			var pixelMessage messages.PixelResultMessage
			if err := json.Unmarshal(raw, &pixelMessage); err == nil {
				c.events <- agentEvent{agent: a, kind: evPixel, pixel: pixelMessage.Data}
			}
		case messages.RenderingCompleted:
			c.events <- agentEvent{agent: a, kind: evCompleted}
		case messages.RenderRequestResponse:
			var resp messages.RenderRequestResponseMessage
			if err := json.Unmarshal(raw, &resp); err == nil && resp.Code != messages.RenderRequestAccepted {
				c.events <- agentEvent{agent: a, kind: evRejected}
			}
		}
	}
}

// assign sends a shard to an agent. Returns false if the write failed.
func (c *coordinator) assign(st *agentState, s shard) bool {
	agentEnv := models.Specification(c.env)
	agentEnv.Image.Patch[2] = s.Min
	agentEnv.Image.Patch[3] = s.Max

	renderRequest := messages.RenderRequestMessage{
		Type:        messages.RenderRequest,
		Data:        agentEnv,
		OperationId: s.ID,
	}

	if err := st.agent.Conn.WriteJSON(renderRequest); err != nil {
		log.Printf("Unable to send render request to agent '%s': %s", st.agent.URL, err)
		return false
	}

	st.busy = true
	st.shard = s
	log.Printf("Assigned columns [%d, %d) to agent '%s'", s.Min, s.Max, st.agent.URL)
	return true
}

// markDead records an agent as gone and returns whether it was alive.
func (c *coordinator) markDead(st *agentState) bool {
	if !st.alive {
		return false
	}
	st.alive = false
	c.liveAgents--
	return true
}

// dispatch hands out pending shards to idle live agents.
func (c *coordinator) dispatch() {
	for len(c.pending) > 0 {
		var target *agentState
		for _, st := range c.states {
			if st.alive && !st.busy {
				target = st
				break
			}
		}
		if target == nil {
			return
		}

		s := c.pending[0]
		c.pending = c.pending[1:]
		if !c.assign(target, s) {
			c.markDead(target)
			// Put the shard back so another agent can pick it up.
			c.pending = append([]shard{s}, c.pending...)
		}
	}
}

// requeueUnfinished splits the not-yet-received columns of a shard into
// contiguous runs and queues them for reassignment.
func (c *coordinator) requeueUnfinished(s shard) {
	for col := s.Min; col < s.Max; {
		if c.colRemaining[col] > 0 {
			start := col
			for col < s.Max && c.colRemaining[col] > 0 {
				col++
			}
			c.pending = append(c.pending, c.newShard(start, col))
		} else {
			col++
		}
	}
}

// anyBusy reports whether any live agent is still working on a shard.
func (c *coordinator) anyBusy() bool {
	for _, st := range c.states {
		if st.alive && st.busy {
			return true
		}
	}
	return false
}

// loop is the master event loop. It returns nil once every column has been
// fully received, or an error if all agents failed before that.
func (c *coordinator) loop() error {
	for c.columnsLeft > 0 {
		if c.liveAgents == 0 {
			return fmt.Errorf("all agents disconnected with %d columns still unrendered", c.columnsLeft)
		}

		// Defensive: if nothing is in flight and nothing is pending but
		// columns are still missing (e.g. an agent reported completion
		// without delivering all pixels), requeue the missing columns.
		if len(c.pending) == 0 && !c.anyBusy() {
			log.Printf("No render work in flight but %d columns are missing; requeueing them", c.columnsLeft)
			c.requeueUnfinished(shard{Min: 0, Max: c.width})
			c.dispatch()
			continue
		}

		ev := <-c.events
		var st *agentState
		for _, s := range c.states {
			if s.agent == ev.agent {
				st = s
				break
			}
		}
		if st == nil {
			continue
		}

		switch ev.kind {
		case evPixel:
			col, row := ev.pixel.I, ev.pixel.J
			if col < 0 || col >= c.width || row < 0 || row >= c.height {
				continue
			}
			if c.received[col][row] {
				// This exact pixel was already written (e.g. a reassigned
				// shard re-rendered rows the dead agent had already
				// delivered). Never write a pixel twice.
				c.stats.DuplicatesDropped++
				continue
			}
			c.received[col][row] = true
			c.colRemaining[col]--
			if c.colRemaining[col] == 0 {
				c.columnsLeft--
			}
			pixel := ev.pixel
			rgbaColor := color.RGBA{pixel.Color[0], pixel.Color[1], pixel.Color[2], 255}
			c.img.Set(col, row, rgbaColor)
			c.progress.Increment()
			c.stats.PixelsWritten++

		case evCompleted:
			// Normal completion: the agent finished its shard and may
			// accept more work.
			st.busy = false
			log.Printf("Agent '%s' completed its shard", st.agent.URL)
			c.dispatch()

		case evRejected:
			// The agent refused the shard (busy or duplicate); requeue it
			// and offer it to another agent.
			if st.busy {
				log.Printf("Agent '%s' rejected shard [%d, %d); requeueing", st.agent.URL, st.shard.Min, st.shard.Max)
				c.pending = append([]shard{st.shard}, c.pending...)
				st.busy = false
			}
			c.dispatch()

		case evDisconnected:
			// Abnormal disconnect: reassign the agent's unfinished columns
			// to the surviving agents.
			if !c.markDead(st) {
				continue
			}
			log.Printf("Agent '%s' disconnected unexpectedly", st.agent.URL)
			if st.busy {
				c.requeueUnfinished(st.shard)
				c.stats.Reassignments++
				st.busy = false
			}
			c.dispatch()
		}
	}
	return nil
}

// coordinate distributes the columns of env across the connected agents,
// reassigning work when agents disconnect, and fills img. It returns an
// error if the image cannot be completed.
func coordinate(env models.Specification, agents []*Agent, img *image.RGBA) (Stats, error) {
	width := env.Image.Width
	height := env.Image.Height

	rows := env.Image.Patch[1] - env.Image.Patch[0]
	if rows <= 0 || rows > height {
		rows = height
	}

	weights := make([]int, len(agents))
	for i, a := range agents {
		weights[i] = a.Cores
	}
	ranges := PartitionColumns(width, weights)

	c := &coordinator{
		env:          env,
		img:          img,
		width:        width,
		height:       height,
		colRemaining: make([]int, width),
		columnsLeft:  width,
		events:       make(chan agentEvent, 1024),
		progress:     utils.StartProgress(width * height),
	}
	for i := range c.colRemaining {
		c.colRemaining[i] = rows
	}
	c.received = make([][]bool, width)
	for i := range c.received {
		c.received[i] = make([]bool, height)
	}

	for _, a := range agents {
		c.states = append(c.states, &agentState{agent: a, alive: true})
		c.liveAgents++
	}

	// Initial shards: one contiguous range per agent. Agents with an empty
	// range (zero cores, or more agents than columns) stay on standby and
	// pick up reassigned work later.
	for _, r := range ranges {
		if r[1] > r[0] {
			c.pending = append(c.pending, c.newShard(r[0], r[1]))
		}
	}

	for _, st := range c.states {
		go c.readPump(st.agent)
	}

	c.dispatch()
	err := c.loop()
	c.progress.Finish()
	return c.stats, err
}

// connectAgents probes every agent URL and returns the ones that are
// reachable, idle and accepted a websocket connection.
func connectAgents(agentURLs []string) []*Agent {
	myClient := &http.Client{
		Timeout: time.Second * 60,
	}

	connectedAgents := []*Agent{}

	for _, agent := range agentURLs {
		wsURL := fmt.Sprintf("ws://%s", agent)
		httpURL := fmt.Sprintf("http://%s/status", agent)

		response, err := myClient.Get(httpURL)
		if err != nil {
			log.Printf("Agent '%s' is unavailable. \n\nError: %s\n", agent, err.Error())
			continue
		}

		var agentStatus messages.AgentStatus
		json.NewDecoder(response.Body).Decode(&agentStatus)
		response.Body.Close()

		if !agentStatus.Available {
			log.Printf("Agent '%s' is busy\n", agent)
			continue
		}

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("Unable to dial ws connection to agent %s with error %s\n", agent, err)
			continue
		}

		// Answer the agent's ping messages so it does not consider us dead.
		conn.SetPingHandler(
			func(appData string) error {
				err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(pongTimeout))
				if err == websocket.ErrCloseSent {
					return nil
				} else if e, ok := err.(net.Error); ok && e.Temporary() {
					return nil
				}
				return err
			},
		)

		connectedAgents = append(connectedAgents, &Agent{
			URL:   agent,
			Cores: agentStatus.Cores,
			Conn:  conn,
		})
	}

	return connectedAgents
}

// run executes the full master workflow. The output PNG is only written
// when every column of the image has been rendered; otherwise an error is
// returned and no (partial) output file is created.
func run(specPath, agentsPath string) error {
	specFile, err := ioutil.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("render spec file error: %v", err)
	}

	agentsData, err := ioutil.ReadFile(agentsPath)
	if err != nil {
		return fmt.Errorf("agents list file error: %v", err)
	}

	agents := make([]string, 0)
	if err := json.Unmarshal(agentsData, &agents); err != nil {
		return fmt.Errorf("unable to parse agents file: %v", err)
	}

	var env models.Specification
	if err := json.Unmarshal(specFile, &env); err != nil {
		return fmt.Errorf("unable to parse render spec: %v", err)
	}

	connectedAgents := connectAgents(agents)
	if len(connectedAgents) == 0 {
		return fmt.Errorf("unable to connect to any agent, please try again later")
	}
	defer func() {
		for _, a := range connectedAgents {
			a.Conn.Close()
		}
	}()

	pngImage := image.NewRGBA(image.Rectangle{
		image.Point{0, 0},
		image.Point{env.Image.Width, env.Image.Height},
	})

	stats, err := coordinate(env, connectedAgents, pngImage)
	if err != nil {
		return err
	}

	log.Printf("Render complete: %d pixels written, %d duplicate pixels dropped, %d shard reassignments",
		stats.PixelsWritten, stats.DuplicatesDropped, stats.Reassignments)

	pngFile := utils.CreateNestedFile(env.Image.OutputFile)
	defer pngFile.Close()

	return png.Encode(pngFile, pngImage)
}

func main() {
	flag.Parse()

	if err := run(renderSpecFile, agentsFile); err != nil {
		log.Fatalf("Render failed: %v", err)
	}
}
