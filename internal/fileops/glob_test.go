package fileops

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

func TestGlobContextSkipsMatchesOutsideAllowedRoots(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.txt"), []byte("escape"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	previousRoots := AllowedRoots
	AllowedRoots = []string{allowed}
	t.Cleanup(func() {
		AllowedRoots = previousRoots
	})

	result, err := GlobContext(context.Background(), &protocol.FileGlobPayload{
		CWD:     allowed,
		Pattern: "../outside/*.txt",
	})
	if err != nil {
		t.Fatalf("GlobContext returned error: %v", err)
	}
	if len(result.Paths) != 0 {
		t.Fatalf("expected outside matches to be filtered out, got %v", result.Paths)
	}
}

func TestGlobContextRecursiveSkipsSymlinkMatchesOutsideAllowedRoots(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(allowed, "secret-link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	previousRoots := AllowedRoots
	AllowedRoots = []string{allowed}
	t.Cleanup(func() {
		AllowedRoots = previousRoots
	})

	result, err := GlobContext(context.Background(), &protocol.FileGlobPayload{
		CWD:     allowed,
		Pattern: "**/*.txt",
	})
	if err != nil {
		t.Fatalf("GlobContext returned error: %v", err)
	}
	if len(result.Paths) != 0 {
		t.Fatalf("expected recursive glob to skip symlink matches outside allowed roots, got %v", result.Paths)
	}
}

func TestWriteContextAllowsNewNestedPathWithinAllowedRoot(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}

	previousRoots := AllowedRoots
	AllowedRoots = []string{allowed}
	t.Cleanup(func() {
		AllowedRoots = previousRoots
	})

	target := filepath.Join(allowed, "nested", "deeper", "file.txt")
	result, err := WriteContext(context.Background(), &protocol.FileWritePayload{
		Path:    target,
		Content: base64.StdEncoding.EncodeToString([]byte("nested content")),
	})
	if err != nil {
		t.Fatalf("WriteContext returned error: %v", err)
	}
	if result.BytesWritten != len("nested content") {
		t.Fatalf("expected %d bytes written, got %d", len("nested content"), result.BytesWritten)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read nested target: %v", err)
	}
	if string(data) != "nested content" {
		t.Fatalf("unexpected nested file content: %q", string(data))
	}
}

func TestMkdirContextAllowsNewNestedPathWithinAllowedRoot(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}

	previousRoots := AllowedRoots
	AllowedRoots = []string{allowed}
	t.Cleanup(func() {
		AllowedRoots = previousRoots
	})

	target := filepath.Join(allowed, "nested", "deeper", "dir")
	if err := MkdirContext(context.Background(), &protocol.FileMkdirPayload{Path: target, Recursive: true}); err != nil {
		t.Fatalf("MkdirContext returned error: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat nested dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", target)
	}
}
