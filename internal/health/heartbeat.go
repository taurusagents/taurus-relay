package health

import (
	"encoding/json"
	"time"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// HeartbeatLoop sends periodic heartbeats on the provided channel.
// It stops when the stop channel is closed.
func HeartbeatLoop(interval time.Duration, infoFn func() *protocol.HeartbeatPayload, send chan<- *protocol.Message, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			info := infoFn()
			payload, _ := json.Marshal(info)
			send <- &protocol.Message{
				Type:    protocol.TypeHeartbeat,
				Payload: payload,
			}
		}
	}
}
