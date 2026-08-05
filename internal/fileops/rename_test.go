package fileops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// The drive-trash move on agent deletion. Doing it in-process is what lets the
// relay stop exec'ing an `mv` that would need the relay's capabilities to be
// inherited by children.
func TestRenameContextMovesAnAgentDriveDirIntoTrash(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	agentDir := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1")
	if err := EnsureOwnedDir(filepath.Join(agentDir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "workspace", "MEMORY.md"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	trashEntry := filepath.Join(dataRoot, DrivesTrashDirName, "1700000000000__user-1__agent-1")
	if err := EnsureOwnedDir(filepath.Dir(trashEntry), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RenameContext(context.Background(), &protocol.FileRenamePayload{From: agentDir, To: trashEntry}); err != nil {
		t.Fatalf("RenameContext: %v", err)
	}

	if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the source to be gone, got %v", err)
	}
	content, err := os.ReadFile(filepath.Join(trashEntry, "workspace", "MEMORY.md"))
	if err != nil || string(content) != "precious" {
		t.Fatalf("expected the whole tree to move, got %q %v", content, err)
	}
	// rename(2) preserves ownership; the trashed tree stays the drive owner's.
	assertOwnedOnDisk(t, trashEntry, remapBase)
}

// `mv -n` exits 0 without moving anything when the destination exists, which is
// why the daemon used to re-stat the source afterwards. This must be an error.
func TestRenameContextRefusesToClobberAnExistingDestination(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	from := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1")
	to := filepath.Join(dataRoot, DrivesTrashDirName, "already-there")
	if err := EnsureOwnedDir(from, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnedDir(to, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "keep.txt"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := RenameContext(context.Background(), &protocol.FileRenamePayload{From: from, To: to})
	if err == nil {
		t.Fatal("expected renaming over an existing destination to fail")
	}
	if !strings.Contains(err.Error(), "existing path") {
		t.Fatalf("expected a clobber-refusal error, got: %v", err)
	}
	if _, statErr := os.Lstat(from); statErr != nil {
		t.Fatalf("the source must be left alone after a refused rename: %v", statErr)
	}
	content, readErr := os.ReadFile(filepath.Join(to, "keep.txt"))
	if readErr != nil || string(content) != "existing" {
		t.Fatalf("the destination must be untouched, got %q %v", content, readErr)
	}
}

func TestRenameContextFailsOnAMissingSource(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	err := RenameContext(context.Background(), &protocol.FileRenamePayload{
		From: filepath.Join(dataRoot, DrivesDirName, "user-1", "never-existed"),
		To:   filepath.Join(dataRoot, DrivesTrashDirName, "entry"),
	})
	// The daemon stats the source first and treats ENOENT as "nothing to trash";
	// the verb itself must not paper over it.
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("expected a not-exist error, got %v", err)
	}
}

func TestRenameContextRefusesPathsOutsideAllowedRoots(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	from := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1")
	if err := EnsureOwnedDir(from, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RenameContext(context.Background(), &protocol.FileRenamePayload{From: from, To: "/tmp/escaped-agent"}); err == nil {
		t.Fatal("expected a destination outside the allowed roots to be refused")
	}
	if _, err := os.Lstat("/tmp/escaped-agent"); !os.IsNotExist(err) {
		t.Fatalf("expected nothing to be created outside the allowed roots, got %v", err)
	}
	if _, err := os.Lstat(from); err != nil {
		t.Fatalf("the source must be left alone: %v", err)
	}
}
