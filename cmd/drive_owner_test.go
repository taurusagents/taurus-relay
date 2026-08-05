package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/fileops"
)

func TestResolveDriveOwnerPrefersFlagOverEnv(t *testing.T) {
	owner, err := resolveDriveOwner("100000:100000", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner == nil || owner.String() != "100000:100000" {
		t.Fatalf("expected the flag to win, got %v", owner)
	}
}

func TestResolveDriveOwnerFallsBackToEnv(t *testing.T) {
	owner, err := resolveDriveOwner("", "165536:165536")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner == nil || owner.String() != "165536:165536" {
		t.Fatalf("expected the env value, got %v", owner)
	}
}

// Node mode must refuse to start when nobody said what the drive owner is. On a
// userns-remapped node an implicit default would create drive directories owned
// by the relay's own uid, which the agent container cannot write — the silent
// failure this whole change exists to remove.
func TestResolveDriveOwnerRefusesUnsetConfiguration(t *testing.T) {
	_, err := resolveDriveOwner("", "")
	if err == nil {
		t.Fatal("expected an unset drive owner to be an error")
	}
	if !strings.Contains(err.Error(), EnvDriveOwner) {
		t.Fatalf("expected the error to name %s, got: %v", EnvDriveOwner, err)
	}
	if !strings.Contains(err.Error(), fileops.DriveOwnerNone) {
		t.Fatalf("expected the error to offer the explicit opt-out, got: %v", err)
	}
}

func TestResolveDriveOwnerRejectsGarbageWithSourceContext(t *testing.T) {
	_, err := resolveDriveOwner("", "dockremap")
	if err == nil {
		t.Fatal("expected a garbage drive owner to be an error")
	}
	if !strings.Contains(err.Error(), EnvDriveOwner) {
		t.Fatalf("expected the error to name the configuration source, got: %v", err)
	}

	_, err = resolveDriveOwner("100000", "")
	if err == nil {
		t.Fatal("expected a uid-only drive owner to be an error")
	}
	if !strings.Contains(err.Error(), "--drive-owner") {
		t.Fatalf("expected the error to name the flag, got: %v", err)
	}
}

func TestValidateDriveOwnerPlatform(t *testing.T) {
	owner, err := fileops.ParseDriveOwner("100000:100000")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDriveOwnerPlatform(owner, "linux"); err != nil {
		t.Fatalf("linux must be supported: %v", err)
	}
	if err := validateDriveOwnerPlatform(owner, "darwin"); err == nil {
		t.Fatal("expected a drive owner to be refused off Linux")
	}
	// The opt-out stays portable so a macOS/Windows relay is never blocked.
	if err := validateDriveOwnerPlatform(nil, "darwin"); err != nil {
		t.Fatalf("the none opt-out must stay portable: %v", err)
	}
}

// End-to-end startup path: parsing, installation and the on-disk self-check.
func TestConfigureDriveOwnershipInstallsRootsAndVerifies(t *testing.T) {
	dataPath := t.TempDir()
	t.Cleanup(func() { fileops.SetDriveOwnership(nil, nil) })

	self := strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	if err := configureDriveOwnership(self, dataPath); err != nil {
		t.Fatalf("configureDriveOwnership: %v", err)
	}

	if got := fileops.DriveOwnerSetting(); got != self {
		t.Fatalf("expected the published drive owner %q, got %q", self, got)
	}
	for _, root := range []string{
		filepath.Join(dataPath, fileops.DrivesDirName),
		filepath.Join(dataPath, fileops.DrivesTrashDirName),
	} {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Fatalf("expected %s to be created by the startup check: %v", root, err)
		}
	}
}

func TestConfigureDriveOwnershipRefusesUnsetOwner(t *testing.T) {
	t.Setenv(EnvDriveOwner, "")
	t.Cleanup(func() { fileops.SetDriveOwnership(nil, nil) })

	if err := configureDriveOwnership("", t.TempDir()); err == nil {
		t.Fatal("expected node startup to refuse an unset drive owner")
	}
}

func TestConfigureDriveOwnershipHonorsExplicitOptOut(t *testing.T) {
	dataPath := t.TempDir()
	t.Cleanup(func() { fileops.SetDriveOwnership(nil, nil) })

	if err := configureDriveOwnership(fileops.DriveOwnerNone, dataPath); err != nil {
		t.Fatalf("expected the none opt-out to start cleanly: %v", err)
	}
	if got := fileops.DriveOwnerSetting(); got != fileops.DriveOwnerNone {
		t.Fatalf("expected the published owner to be %q, got %q", fileops.DriveOwnerNone, got)
	}
	// Opting out must not create or touch anything on disk.
	if _, err := os.Stat(filepath.Join(dataPath, fileops.DrivesDirName)); !os.IsNotExist(err) {
		t.Fatalf("expected no drive roots to be created for an opted-out node, got %v", err)
	}
}
