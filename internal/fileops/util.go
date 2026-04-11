package fileops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AllowedRoots restricts file operations to these directory trees.
// Set during initialization (e.g., to the user's home directory).
// If empty, no restrictions are applied.
var AllowedRoots []string

// expandPath expands ~ to home directory and resolves relative paths.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// ValidatePath resolves symlinks and verifies the path is under an allowed root.
// Returns the resolved absolute path or an error.
func ValidatePath(path string) (string, error) {
	expanded := expandPath(path)

	// Resolve symlinks
	resolved, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		// File might not exist yet (for writes) — resolve parent dir
		dir := filepath.Dir(expanded)
		resolvedDir, err2 := filepath.EvalSymlinks(dir)
		if err2 != nil {
			return "", fmt.Errorf("cannot resolve path: %w", err2)
		}
		resolved = filepath.Join(resolvedDir, filepath.Base(expanded))
	}

	if len(AllowedRoots) == 0 {
		return resolved, nil // no restrictions configured
	}

	for _, root := range AllowedRoots {
		if strings.HasPrefix(resolved, root+"/") || resolved == root {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q is outside allowed roots", path)
}
