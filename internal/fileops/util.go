package fileops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AllowedRoots restricts file operations to these directory trees.
// Set during initialization (e.g., to the user's home directory).
// If empty, no restrictions are applied.
var AllowedRoots []string

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

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
		resolved, err = resolvePathFromExistingAncestor(expanded)
		if err != nil {
			return "", err
		}
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

func resolvePathFromExistingAncestor(path string) (string, error) {
	current := path
	remainder := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(remainder) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, remainder[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve path: %w", err)
		}
		remainder = append(remainder, filepath.Base(current))
		current = parent
	}
}
