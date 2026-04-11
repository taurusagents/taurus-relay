// Package auth handles token-based registration and JWT refresh.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// BuildRegistrationMessage creates the `type: "auth"` message for initial registration.
func BuildRegistrationMessage(token string) (*protocol.Message, error) {
	hostname, _ := os.Hostname()

	payload := protocol.AuthRegistrationPayload{
		RegistrationToken: token,
		Hostname:          hostname,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		RelayVersion:      health.Version,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal registration payload: %w", err)
	}

	return &protocol.Message{
		ID:      "auth-register",
		Type:    protocol.TypeAuth,
		Payload: data,
	}, nil
}

// BuildReconnectMessage creates the `type: "auth"` message for JWT reconnection.
func BuildReconnectMessage(jwt string) (*protocol.Message, error) {
	payload := protocol.AuthReconnectPayload{
		JWT: jwt,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal reconnect payload: %w", err)
	}

	return &protocol.Message{
		ID:      "auth-reconnect",
		Type:    protocol.TypeAuth,
		Payload: data,
	}, nil
}

// HandleAuthResult processes the `auth.result` response and saves credentials if present.
func HandleAuthResult(msg *protocol.Message, cfg *config.Config) error {
	var result protocol.AuthResultPayload
	if err := json.Unmarshal(msg.Payload, &result); err != nil {
		return fmt.Errorf("parse auth result: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("auth failed: %s", result.Error)
	}

	if result.TargetID != "" {
		cfg.TargetID = result.TargetID
	}

	// Save JWT if the server returned one (registration flow)
	if result.JWT != "" {
		cfg.JWT = result.JWT
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
	}

	return nil
}
