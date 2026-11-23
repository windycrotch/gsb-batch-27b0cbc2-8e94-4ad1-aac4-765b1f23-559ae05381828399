package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"net/http"

	"github.com/DheerendraRathor/GoTracer/models"
	"github.com/DheerendraRathor/GoTracer/net/constants"
	"github.com/gorilla/websocket"
)

func renderSpec(width, height int) models.Specification {
	var env models.Specification
	env.Settings.RenderRoutines = 2
	env.Settings.RenderDepth = 1
	env.Image.Width = width
	env.Image.Height = height
	env.Image.Samples = 1
	env.Image.Patch = [4]int{0, height, 0, width}
	return env
}

func sendRenderRequest(t *testing.T, conn *websocket.Conn, operationId string, spec models.Specification) {
	t.Helper()
	err := conn.WriteJSON(messages.RenderRequestMessage{
		Type:        messages.RenderRequest,
		Data:        spec,
		OperationId: operationId,
	})
	if err != nil {
		t.Fatalf("unable to send render request: %v", err)
	}
}

type agentMessage struct {
	response *messages.RenderRequestResponseMessage
	pixel    *messages.PixelResultMessage
	done     bool
}

func readAgentMessage(t *testing.T, conn *websocket.Conn) agentMessage {
	t.Helper()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("unable to read agent message: %v", err)
	}

	var msg messages.WebSocketMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unable to decode agent message: %v", err)
	}

	switch msg.Type {
	case messages.RenderRequestResponse:
		var resp messages.RenderRequestResponseMessage
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("unable to decode render response: %v", err)
		}
		return agentMessage{response: &resp}
	case messages.PixelResult:
		var pixel messages.PixelResultMessage
		if err := json.Unmarshal(raw, &pixel); err != nil {
			t.Fatalf("unable to decode pixel: %v", err)
		}
		return agentMessage{pixel: &pixel}
	case messages.RenderingCompleted:
		return agentMessage{done: true}
	default:
		t.Fatalf("unexpected message type from agent: %q", msg.Type)
		return agentMessage{}
	}
}

// TestAgentBusyDuplicateAndReuse verifies that the agent:
//  1. accepts a first render request,
//  2. rejects a second request while the first is still running, and keeps
//     streaming the first task's results,
//  3. rejects a re-dispatched shard (same OperationId) as a duplicate,
//  4. accepts new work again after the previous task completed.
func TestAgentBusyDuplicateAndReuse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", agentHandler)
	mux.HandleFunc("/status", agentStatusHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("unable to dial agent: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	big := renderSpec(200, 200) // 40000 pixels: slow enough to still be running
	small := renderSpec(2, 2)

	// 1. First request is accepted.
	sendRenderRequest(t, conn, "op-1", big)
	// 2. Second request arrives while the first is still rendering.
	sendRenderRequest(t, conn, "op-2", small)

	var accepted, busyRejected bool
	pixelCount := 0
	completed := false
	for !completed {
		m := readAgentMessage(t, conn)
		switch {
		case m.response != nil:
			switch m.response.OperationId {
			case "op-1":
				if m.response.Code != messages.RenderRequestAccepted {
					t.Fatalf("op-1: expected %q, got %q", messages.RenderRequestAccepted, m.response.Code)
				}
				accepted = true
			case "op-2":
				if m.response.Code != messages.RenderRequestTracingAlreadyInProgress {
					t.Fatalf("op-2: expected %q, got %q", messages.RenderRequestTracingAlreadyInProgress, m.response.Code)
				}
				busyRejected = true
			default:
				t.Fatalf("unexpected response for operation %q", m.response.OperationId)
			}
		case m.pixel != nil:
			if m.pixel.OperationId != "op-1" {
				t.Fatalf("received pixel for unexpected operation %q", m.pixel.OperationId)
			}
			pixelCount++
		case m.done:
			completed = true
		}
	}

	if !accepted {
		t.Fatal("never received an accept response for op-1")
	}
	if !busyRejected {
		t.Fatal("op-2 was not rejected while op-1 was in progress")
	}
	if pixelCount != 200*200 {
		t.Fatalf("expected %d pixels from op-1, got %d", 200*200, pixelCount)
	}

	// 3. Re-dispatching the same shard (same OperationId) is rejected as a
	// duplicate and does not produce any new pixels.
	sendRenderRequest(t, conn, "op-1", big)
	m := readAgentMessage(t, conn)
	if m.response == nil || m.response.Code != messages.RenderRequestDuplicate {
		t.Fatalf("expected duplicate rejection for re-dispatched op-1, got %+v", m)
	}

	// 4. After completion the agent accepts new work again.
	sendRenderRequest(t, conn, "op-3", small)
	smallPixels := 0
	smallDone := false
	smallAccepted := false
	for !smallDone {
		m := readAgentMessage(t, conn)
		switch {
		case m.response != nil:
			if m.response.OperationId != "op-3" || m.response.Code != messages.RenderRequestAccepted {
				t.Fatalf("op-3: expected accept, got %+v", m.response)
			}
			smallAccepted = true
		case m.pixel != nil:
			if m.pixel.OperationId != "op-3" {
				t.Fatalf("received pixel for unexpected operation %q", m.pixel.OperationId)
			}
			smallPixels++
		case m.done:
			smallDone = true
		}
	}
	if !smallAccepted {
		t.Fatal("op-3 was not accepted after op-1 completed")
	}
	if smallPixels != 4 {
		t.Fatalf("expected 4 pixels from op-3, got %d", smallPixels)
	}
}
