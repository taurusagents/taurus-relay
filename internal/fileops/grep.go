package fileops

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

const maxGrepMatches = 500

// Grep searches file contents using regex. Uses ripgrep if available, falls back to Go.
func Grep(p *protocol.FileGrepPayload) (*protocol.FileGrepResultPayload, error) {
	searchPath := p.Path
	if searchPath == "" {
		searchPath = "."
	}

	// Validate the search path
	searchPath, err := ValidatePath(searchPath)
	if err != nil {
		return nil, err
	}

	// Try ripgrep first
	if rgPath, err := exec.LookPath("rg"); err == nil {
		return grepRipgrep(rgPath, p.Pattern, searchPath, p.Glob)
	}

	// Fallback to Go regex
	return grepGo(p.Pattern, searchPath, p.Glob)
}

func grepRipgrep(rgPath, pattern, searchPath, glob string) (*protocol.FileGrepResultPayload, error) {
	args := []string{
		"--line-number",
		"--no-heading",
		"--max-count", "100",
		"--max-filesize", "1M",
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	args = append(args, pattern, searchPath)

	cmd := exec.Command(rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// rg returns exit code 1 for no matches
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return &protocol.FileGrepResultPayload{Matches: []protocol.GrepMatch{}}, nil
		}
		// exit code 2 = error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return nil, fmt.Errorf("ripgrep error: %s", string(exitErr.Stderr))
		}
	}

	var matches []protocol.GrepMatch
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() && len(matches) < maxGrepMatches {
		line := scanner.Text()
		// Format: file:line:text
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		lineNum, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		matches = append(matches, protocol.GrepMatch{
			File: parts[0],
			Line: lineNum,
			Text: parts[2],
		})
	}

	return &protocol.FileGrepResultPayload{Matches: matches}, nil
}

func grepGo(pattern, searchPath, globFilter string) (*protocol.FileGrepResultPayload, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}

	var matches []protocol.GrepMatch

	err = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() {
				base := info.Name()
				if base == "node_modules" || base == ".git" || base == "__pycache__" || base == ".venv" {
					return filepath.SkipDir
				}
			}
			return nil
		}

		if len(matches) >= maxGrepMatches {
			return filepath.SkipAll
		}

		// Skip binary/large files
		if info.Size() > 1024*1024 {
			return nil
		}

		// Apply glob filter
		if globFilter != "" {
			matched, _ := filepath.Match(globFilter, filepath.Base(path))
			if !matched {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() && len(matches) < maxGrepMatches {
			lineNum++
			text := scanner.Text()
			if re.MatchString(text) {
				matches = append(matches, protocol.GrepMatch{
					File: path,
					Line: lineNum,
					Text: text,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &protocol.FileGrepResultPayload{Matches: matches}, nil
}
