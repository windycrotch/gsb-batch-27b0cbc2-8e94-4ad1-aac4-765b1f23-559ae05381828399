package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"strings"

	"runtime"

	"github.com/DheerendraRathor/GoTracer/models"
	"github.com/DheerendraRathor/GoTracer/net/constants"
	"github.com/DheerendraRathor/GoTracer/tracer"
	"github.com/gorilla/websocket"
)

const (
	pongTimeout = 60 * time.Second
	pongWait    = 20 * time.Second
	pingMessage = "a"
)

type RenderingClient struct {
	Conn        *websocket.Conn
	Results     chan *models.Pixel
	CloseChan   chan struct{}
	PongChannel chan bool
	WaitGroup   sync.WaitGroup

	// mu guards Conn writes (both goroutines below write to the connection)
	// as well as the fields that follow it.
	mu                  sync.Mutex
	isTracingInProgress bool
	operationId         string
	seenOperations      map[string]bool
}

func (c *RenderingClient) Initialize() {
	c.WaitGroup = sync.WaitGroup{}

	c.Conn.SetPongHandler(
		func(message string) error {
			select {
			case c.PongChannel <- true:
			default:
			}
			return nil
		},
	)

	go pingThread(c.Conn, c.PongChannel)

	c.WaitGroup.Add(2)

	go c.ReadHandler()
	go c.ResultSender()

	c.WaitGroup.Wait()
}

// writeJSON serializes writes to the websocket connection; ReadHandler and
// ResultSender both write, and gorilla/websocket allows only one concurrent
// writer.
func (c *RenderingClient) writeJSON(v interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.WriteJSON(v); err != nil {
		log.Printf("Unable to write message to master: %s", err)
	}
}

// newOperationId generates a random operation id without external
// dependencies.
func newOperationId() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("op-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

var addr = flag.String("addr", "", "http service address")

var upgrader = websocket.Upgrader{}
var mutex = &sync.Mutex{}

var activeConn *websocket.Conn = nil

func sendPing(conn *websocket.Conn) error {
	err := conn.WriteControl(websocket.PingMessage, []byte(pingMessage), time.Now().Add(time.Hour))
	return err
}

func pingThread(conn *websocket.Conn, pongChan <-chan bool) {
	ticker := time.NewTicker(pongWait)
	timer := time.NewTimer(pongTimeout)

	for {
		select {
		case <-timer.C:
			conn.WriteControl(websocket.CloseNoStatusReceived, []byte("No pong received"), time.Now().Add(time.Hour))
			conn.Close()
			return
		case <-ticker.C:
			err := sendPing(conn)
			if err != nil {
				ticker.Stop()
				return
			}
		case <-pongChan:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(pongTimeout)
		}
	}
}

func (c *RenderingClient) ReadHandler() {
	defer func() {
		c.WaitGroup.Done()
		// Signal the tracer and ResultSender that the connection is gone.
		close(c.CloseChan)
	}()

	var message messages.WebSocketMessage
	for {
		_, msgStr, err := c.Conn.ReadMessage()
		if err != nil {
			log.Printf("Unable to understand message: %s", err)
			return
		}

		if err := json.Unmarshal(msgStr, &message); err != nil {
			continue
		}

		if message.Type != messages.RenderRequest {
			continue
		}

		// Fresh variable per request: the tracer goroutine reads Data while
		// the next message is being decoded.
		var renderReqMsg messages.RenderRequestMessage
		if err := json.Unmarshal(msgStr, &renderReqMsg); err != nil {
			continue
		}

		operationId := strings.TrimSpace(renderReqMsg.OperationId)
		if operationId == "" {
			operationId = newOperationId()
		}

		c.mu.Lock()
		var code messages.RenderResponseCode
		switch {
		case c.seenOperations[operationId]:
			// The same shard was dispatched twice. Do not render it again:
			// re-rendering would make two agents (or two runs) stream the
			// same columns and overwrite each other's pixels.
			code = messages.RenderRequestDuplicate
		case c.isTracingInProgress:
			// A previous render is still running. Reject the new request;
			// the old task keeps streaming its results undisturbed.
			code = messages.RenderRequestTracingAlreadyInProgress
		default:
			c.seenOperations[operationId] = true
			c.isTracingInProgress = true
			c.operationId = operationId
			code = messages.RenderRequestAccepted
			go tracer.GoTrace(&renderReqMsg.Data, true, c.Results, true, c.CloseChan)
		}
		c.mu.Unlock()

		c.writeJSON(messages.RenderRequestResponseMessage{
			Type:        messages.RenderRequestResponse,
			Code:        code,
			OperationId: operationId,
		})
	}
}

func (c *RenderingClient) ResultSender() {
	defer c.WaitGroup.Done()

	for {
		select {
		case <-c.CloseChan:
			return
		case pixel := <-c.Results:
			c.mu.Lock()
			operationId := c.operationId
			c.mu.Unlock()

			if pixel == nil {
				// The current render finished: notify the master and allow
				// the next render request to be accepted.
				c.writeJSON(messages.WebSocketMessage{
					Type:        messages.RenderingCompleted,
					OperationId: operationId,
				})
				c.mu.Lock()
				c.isTracingInProgress = false
				c.mu.Unlock()
				continue
			}

			c.writeJSON(messages.WebSocketMessage{
				Type:        messages.PixelResult,
				Data:        pixel,
				OperationId: operationId,
			})
		}
	}
}

func agentHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received a connection request")

	mutex.Lock()
	if activeConn != nil {
		mutex.Unlock()
		log.Print("An active connection already exists")
		http.Error(w, "An active connection already exists", http.StatusConflict)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		mutex.Unlock()
		log.Print("upgrade:", err)
		return
	}
	activeConn = conn
	mutex.Unlock()

	defer func() {
		mutex.Lock()
		activeConn = nil
		mutex.Unlock()
		conn.Close()
	}()

	client := RenderingClient{
		Conn:           conn,
		PongChannel:    make(chan bool, 1),
		Results:        make(chan *models.Pixel, 100),
		CloseChan:      make(chan struct{}),
		seenOperations: make(map[string]bool),
	}

	client.Initialize()
}

func agentStatusHandler(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	available := activeConn == nil
	mutex.Unlock()

	status := messages.AgentStatus{
		Available: available,
		Cores:     runtime.NumCPU(),
	}

	jsonStatus, err := json.Marshal(status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonStatus)
}

func main() {
	flag.Parse()

	if *addr == "" {
		flag.Usage()
		log.Fatalf("Agent address \"%s\" is invalid.", *addr)
	}

	log.SetFlags(0)
	http.HandleFunc("/", agentHandler)
	http.HandleFunc("/status", agentStatusHandler)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
