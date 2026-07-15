package config

import (
	"os"
	"path/filepath"
	"testing"
)

// resetDir clears the flag override after a test so tests stay independent.
func resetDir(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { SetDir("") })
}

func TestPathDefault(t *testing.T) {
	resetDir(t)
	t.Setenv(EnvConfigDir, "")

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	want := filepath.Join(home, ".config", "taurus-relay", "config.json")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathEnvOverridesDefault(t *testing.T) {
	resetDir(t)
	envDir := t.TempDir()
	t.Setenv(EnvConfigDir, envDir)

	want := filepath.Join(envDir, "config.json")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathFlagOverridesEnv(t *testing.T) {
	resetDir(t)
	envDir := t.TempDir()
	flagDir := t.TempDir()
	t.Setenv(EnvConfigDir, envDir)
	SetDir(flagDir)

	want := filepath.Join(flagDir, "config.json")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestSetDirEmptyClearsOverride(t *testing.T) {
	resetDir(t)
	envDir := t.TempDir()
	t.Setenv(EnvConfigDir, envDir)

	SetDir(t.TempDir())
	SetDir("")

	// With the flag override cleared, the env var should win again.
	want := filepath.Join(envDir, "config.json")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLoadSaveRoundTripInOverrideDir(t *testing.T) {
	resetDir(t)
	dir := t.TempDir()
	SetDir(dir)

	// Empty override dir means "fresh config": Load returns a zero config.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() in empty dir: %v", err)
	}
	if cfg.HasCredentials() {
		t.Fatal("expected zero config from empty dir")
	}

	cfg.Server = "https://example.test"
	cfg.TargetID = "target-1"
	cfg.JWT = "jwt-1"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.json not written to override dir: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() after Save: %v", err)
	}
	if loaded.Server != "https://example.test" || loaded.TargetID != "target-1" || loaded.JWT != "jwt-1" {
		t.Errorf("round trip mismatch: %+v", loaded)
	}
}
