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
	// Intermediates keep the standard 0755 so container root can traverse them —
	// inside a drive root that must not depend on the relay unit's umask.
	parent, err := os.Stat(filepath.Dir(leaf))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0o755 {
		t.Fatalf("expected intermediate mode 0755, got %04o", parent.Mode().Perm())
	}
}

// Outside the managed drive roots (connect-mode relays, and the node's own
// runtime staging paths) the process umask keeps applying exactly as it did when
// this was a plain os.MkdirAll.
func TestEnsureOwnedDirRespectsUmaskOutsideDriveRoots(t *testing.T) {
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)

	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	dir := filepath.Join(dataRoot, "runtime", "seccomp")
	if err := EnsureOwnedDir(dir, 0); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected the umask to still apply outside drive roots (0700), got %04o", info.Mode().Perm())
	}
}
