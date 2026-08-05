package tunnel

import (
	"path/filepath"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

func TestBuildNodeRegisterMetaPublishesCurrentDriveRootMetadata(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), ".", "node-data")
	meta := buildNodeRegisterMeta(dataPath, &protocol.HeartbeatPayload{OS: "linux", Arch: "amd64"}, "node-1", "100000:100000")

	cleanDataRoot := filepath.Clean(dataPath)
	wantDrivePath := filepath.Join(cleanDataRoot, "taurus-drives")
	if got := meta["data_root"]; got != cleanDataRoot {
		t.Fatalf("expected data_root %q, got %q", cleanDataRoot, got)
	}
	if got := meta["taurus_drive_path"]; got != wantDrivePath {
		t.Fatalf("expected taurus_drive_path %q, got %q", wantDrivePath, got)
	}
	if got := meta["taurus_drive_owner"]; got != "100000:100000" {
		t.Fatalf("expected taurus_drive_owner 100000:100000, got %q", got)
	}
	if got := meta["container_count"]; got != "0" {
		t.Fatalf("expected compatibility container_count 0, got %q", got)
	}
	if _, ok := meta["drive_path"]; ok {
		t.Fatalf("expected legacy drive_path metadata to be omitted, got %v", meta)
	}
	if got := meta["os"]; got != "linux" {
		t.Fatalf("expected os linux, got %q", got)
	}
	if got := meta["arch"]; got != "amd64" {
		t.Fatalf("expected arch amd64, got %q", got)
	}
	if got := meta["hostname"]; got != "node-1" {
		t.Fatalf("expected hostname node-1, got %q", got)
	}
}

// A node without userns-remap publishes the explicit opt-out rather than
// omitting the key. The daemon needs to tell "this relay knows about drive
// ownership and is deliberately not rewriting it" apart from "this relay
// predates the feature"; only the latter is allowed to be silent.
func TestBuildNodeRegisterMetaPublishesDriveOwnerOptOut(t *testing.T) {
	meta := buildNodeRegisterMeta(t.TempDir(), &protocol.HeartbeatPayload{OS: "linux", Arch: "amd64"}, "node-2", "none")
	if got := meta["taurus_drive_owner"]; got != "none" {
		t.Fatalf("expected taurus_drive_owner none, got %q", got)
	}
}
