package protocol

import (
	"encoding/json"
	"fmt"
	"log"
)

// Handler processes incoming messages and returns responses.
type Handler struct {
	handlers map[string]HandlerFunc
}

// HandlerFunc processes a message and returns a response payload (or error).
type HandlerFunc func(id string, payload json.RawMessage) (responseType string, responsePayload any, err error)

// NewHandler creates a new message handler.
func NewHandler() *Handler {
	return &Handler{
		handlers: make(map[string]HandlerFunc),
	}
}

// Register adds a handler for a message type.
func (h *Handler) Register(msgType string, fn HandlerFunc) {
	h.handlers[msgType] = fn
}

// Handle processes an incoming message and returns the response Message (or nil for fire-and-forget).
func (h *Handler) Handle(msg *Message) *Message {
	fn, ok := h.handlers[msg.Type]
	if !ok {
		log.Printf("[handler] unknown message type: %s (id=%s)", msg.Type, msg.ID)
		errStr := fmt.Sprintf("unknown message type: %s", msg.Type)
		return &Message{
			ID:    msg.ID,
			Type:  msg.Type + ".error",
			Error: &errStr,
		}
	}

	respType, respPayload, err := fn(msg.ID, msg.Payload)
	if err != nil {
		errStr := err.Error()
		return &Message{
			ID:    msg.ID,
			Type:  respType,
			Error: &errStr,
		}
	}

	if respPayload == nil {
		return nil // no response needed (e.g., pong already sent)
	}

	payloadBytes, err := json.Marshal(respPayload)
	if err != nil {
		errStr := fmt.Sprintf("failed to marshal response: %v", err)
		return &Message{
			ID:    msg.ID,
			Type:  respType,
			Error: &errStr,
		}
	}

	return &Message{
		ID:      msg.ID,
		Type:    respType,
		Payload: payloadBytes,
	}
}
