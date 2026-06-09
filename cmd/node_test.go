package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalizeNodeDataPathResolvesRelativeSymlinkedRoots(t *testing.T) {
	tempRoot := t.TempDir()
	realRoot := filepath.Join(tempRoot, "real", "node-data")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("mkdir real root: %v", err)
	}
	linkPath := filepath.Join(tempRoot, "node-link")
	if err := os.Symlink(realRoot, linkPath); err != nil {
		t.Fatalf("symlink data root: %v", err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(tempRoot); err != nil {
		t.Fatalf("chdir temp root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})

	resolved, err := canonicalizeNodeDataPath(filepath.Join(".", "node-link"))
	if err != nil {
		t.Fatalf("canonicalizeNodeDataPath returned error: %v", err)
	}
	if resolved != realRoot {
		t.Fatalf("expected canonical data path %q, got %q", realRoot, resolved)
	}
}

func TestValidateNodePlatformRejectsWindows(t *testing.T) {
	err := validateNodePlatform("windows")
	if err == nil {
		t.Fatal("expected Windows node mode to be rejected")
	}
	if !strings.Contains(err.Error(), "connect mode only") {
		t.Fatalf("expected clear connect-only Windows error, got %v", err)
	}
}

func TestValidateNodePlatformAllowsLinux(t *testing.T) {
	if err := validateNodePlatform("linux"); err != nil {
		t.Fatalf("expected Linux node mode to remain supported, got %v", err)
	}
}
