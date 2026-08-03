//go:build !windows

package fileops

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The requested mode must survive the process umask, otherwise codex-config
// lands 0755 instead of the 0700 the ownership spec calls for.
func TestEnsureOwnedDirAppliesRequestedLeafModeDespiteUmask(t *testing.T) {
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)

	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	leaf := filepath.Join(dataRoot, DrivesDirName, "user-1", "codex-config")
	if err := EnsureOwnedDir(leaf, 0o700); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}

	info, err := os.Stat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected leaf mode 0700, got %04o", info.Mode().Perm())
	}
	// Intermediates keep the standard 0755 so container root can traverse them.
	parent, err := os.Stat(filepath.Dir(leaf))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0o755 {
		t.Fatalf("expected intermediate mode 0755, got %04o", parent.Mode().Perm())
	}
}
