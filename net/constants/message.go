package messages

import (
	"github.com/DheerendraRathor/GoTracer/models"
)

type RenderResponseCode string

const (
	RenderRequestAccepted                 RenderResponseCode = "RenderRequestAccepted"
	RenderRequestTracingAlreadyInProgress RenderResponseCode = "RenderRequestTracingAlreadyInProgress"
	// RenderRequestDuplicate is returned when the same shard (identified by
	// OperationId) is dispatched to an agent more than once. The agent does
	// not re-render it, so no column is ever returned twice.
	RenderRequestDuplicate RenderResponseCode = "RenderRequestDuplicate"
)

type WebSocketMessage struct {
	Type        string
	Data        interface{}
	OperationId string
}

type RenderRequestMessage struct {
	Type        string
	Data        models.Specification
	OperationId string
}

type RenderRequestResponseMessage struct {
	Type        string
	Code        RenderResponseCode
	OperationId string
}

type PixelResultMessage struct {
	Type        string
	Data        models.Pixel
	OperationId string
}

type AgentStatus struct {
	Available bool
	Cores     int
}
