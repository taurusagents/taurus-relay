package cmd

import (
	"os"
	"path/filepath"
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

	t.Chdir(tempRoot)
	resolved, err := canonicalizeNodeDataPath(filepath.Join(".", "node-link"))
	if err != nil {
		t.Fatalf("canonicalizeNodeDataPath returned error: %v", err)
	}
	if resolved != realRoot {
		t.Fatalf("expected canonical data path %q, got %q", realRoot, resolved)
	}
}
