package tunnel

import (
	"path/filepath"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

func TestBuildNodeRegisterMetaPublishesDriveRootMetadata(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), ".", "node-data")
	meta := buildNodeRegisterMeta(dataPath, &protocol.HeartbeatPayload{OS: "linux", Arch: "amd64"}, "node-1")

	cleanDataRoot := filepath.Clean(dataPath)
	wantDrivePath := filepath.Join(cleanDataRoot, "taurus-drives")
	if got := meta["data_root"]; got != cleanDataRoot {
		t.Fatalf("expected data_root %q, got %q", cleanDataRoot, got)
	}
	if got := meta["drive_path"]; got != wantDrivePath {
		t.Fatalf("expected drive_path %q, got %q", wantDrivePath, got)
	}
	if got := meta["taurus_drive_path"]; got != wantDrivePath {
		t.Fatalf("expected taurus_drive_path %q, got %q", wantDrivePath, got)
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
