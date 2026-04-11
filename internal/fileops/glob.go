package fileops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Glob finds files matching a pattern.
func Glob(p *protocol.FileGlobPayload) (*protocol.FileGlobResultPayload, error) {
	cwd := p.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}

	// Validate the base directory
	cwd, err := ValidatePath(cwd)
	if err != nil {
		return nil, err
	}

	pattern := p.Pattern

	// If pattern contains **, we need recursive walk
	if strings.Contains(pattern, "**") {
		return globRecursive(cwd, pattern)
	}

	// Simple glob
	fullPattern := filepath.Join(cwd, pattern)
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return nil, err
	}

	// Make paths relative to cwd
	result := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(cwd, m)
		if err != nil {
			rel = m
		}
		result = append(result, rel)
	}

	sort.Strings(result)
	return &protocol.FileGlobResultPayload{Paths: result}, nil
}

func globRecursive(root, pattern string) (*protocol.FileGlobResultPayload, error) {
	var result []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		// Skip common vendor directories
		if info.IsDir() {
			base := info.Name()
			if base == "node_modules" || base == ".git" || base == "__pycache__" || base == ".venv" {
				return filepath.SkipDir
			}
		}

		matched, err := matchDoublestar(pattern, rel)
		if err != nil {
			return nil
		}
		if matched {
			result = append(result, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(result)
	return &protocol.FileGlobResultPayload{Paths: result}, nil
}

// matchDoublestar performs simple ** glob matching.
func matchDoublestar(pattern, name string) (bool, error) {
	// Split pattern by **
	parts := strings.Split(pattern, "**")
	if len(parts) == 1 {
		// No **, use standard match
		return filepath.Match(pattern, name)
	}

	// For simple cases like "**/*.go"
	if len(parts) == 2 && parts[0] == "" {
		suffix := strings.TrimPrefix(parts[1], "/")
		if suffix == "" {
			return true, nil
		}
		// Check if any suffix of the path matches
		pathParts := strings.Split(name, string(filepath.Separator))
		for i := range pathParts {
			remaining := strings.Join(pathParts[i:], string(filepath.Separator))
			matched, err := filepath.Match(suffix, remaining)
			if err != nil {
				return false, err
			}
			if matched {
				return true, nil
			}
		}
		// Also try matching just the filename
		return filepath.Match(suffix, filepath.Base(name))
	}

	// Fallback: check if pattern prefix and suffix match
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[len(parts)-1], "/")

	if prefix != "" && !strings.HasPrefix(name, prefix) {
		return false, nil
	}
	if suffix != "" {
		return filepath.Match(suffix, filepath.Base(name))
	}
	return true, nil
}
