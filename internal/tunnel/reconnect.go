// Package tunnel manages the WebSocket connection to the Taurus daemon.
package tunnel

import (
	"math"
	"math/rand"
	"time"
)

// ReconnectConfig controls auto-reconnect behavior.
type ReconnectConfig struct {
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	MaxRetries    int // 0 = infinite
}

// DefaultReconnectConfig returns sensible defaults.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		InitialDelay:  1 * time.Second,
		MaxDelay:      30 * time.Second,
		BackoffFactor: 2.0,
		MaxRetries:    0,
	}
}

// Backoff calculates the delay for a given attempt number, with up to 25% jitter.
func (rc ReconnectConfig) Backoff(attempt int) time.Duration {
	delay := float64(rc.InitialDelay) * math.Pow(rc.BackoffFactor, float64(attempt))
	if delay > float64(rc.MaxDelay) {
		delay = float64(rc.MaxDelay)
	}
	d := time.Duration(delay)
	// Add up to 25% jitter to avoid thundering herd
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	return d + jitter
}
