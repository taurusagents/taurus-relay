package tunnel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/protocol"
)

func TestNodeTunnelAppliesConfiguredMaxSessionsToProcMux(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir(), MaxSessions: 3})
	if tun.procs == nil {
		t.Fatalf("expected node tunnel to build a proc multiplexer")
	}
	if tun.procs.MaxSessions != 3 {
		t.Fatalf("expected proc mux cap 3, got %d", tun.procs.MaxSessions)
	}
}

func TestNodeTunnelZeroMaxSessionsMeansUnlimited(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	if tun.procs.MaxSessions != 0 {
		t.Fatalf("expected unlimited (0) proc mux cap, got %d", tun.procs.MaxSessions)
	}
}

func TestConnectTunnelAppliesMaxSessionsToBothMuxes(t *testing.T) {
	tun := New(&config.Config{Server: "https://example.com"}, "", 5)
	if tun.shells == nil || tun.procs == nil {
		t.Fatalf("expected connect tunnel to build shell and proc multiplexers")
	}
	if tun.shells.MaxSessions != 5 {
		t.Fatalf("expected shell mux cap 5, got %d", tun.shells.MaxSessions)
	}
	if tun.procs.MaxSessions != 5 {
		t.Fatalf("expected proc mux cap 5, got %d", tun.procs.MaxSessions)
	}
}

func TestMaxSessionsSurvivesRuntimeReset(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir(), MaxSessions: 9})
	tun.resetRuntimeState()
	if tun.procs.MaxSessions != 9 {
		t.Fatalf("expected rebuilt proc mux to keep cap 9, got %d", tun.procs.MaxSessions)
	}
}

func TestNodeRegisterPayloadAdvertisesMaxProcSessions(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{
		Name:        "node-1",
		Host:        "203.0.113.10",
		DataPath:    t.TempDir(),
		MaxSessions: 4096,
	})
	payload := tun.buildNodeRegisterPayload(8, 4, &protocol.HeartbeatPayload{OS: "linux", Arch: "amd64"}, "node-1")
	if payload.MaxProcSessions != 4096 {
		t.Fatalf("expected max proc sessions 4096, got %d", payload.MaxProcSessions)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(data), `"max_proc_sessions":4096`) {
		t.Fatalf("expected max_proc_sessions on the wire, got %s", data)
	}
}

func TestNodeRegisterPayloadOmitsMaxProcSessionsWhenUnlimited(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{
		Name:     "node-1",
		Host:     "203.0.113.10",
		DataPath: t.TempDir(),
	})
	payload := tun.buildNodeRegisterPayload(8, 4, &protocol.HeartbeatPayload{OS: "linux", Arch: "amd64"}, "node-1")
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	// Absent field keeps the wire shape identical to older relays, which is
	// what lets old daemons keep working unchanged.
	if strings.Contains(string(data), "max_proc_sessions") {
		t.Fatalf("expected unlimited cap to be omitted from the wire, got %s", data)
	}
}
