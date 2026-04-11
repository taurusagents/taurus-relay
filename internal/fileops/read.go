// Package fileops implements structured file operations.
package fileops

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Read reads a file and returns its content as base64.
// offset and limit are in lines (1-based), 0 means no limit.
func Read(p *protocol.FileReadPayload) (*protocol.FileReadResultPayload, error) {
	path, err := ValidatePath(p.Path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", p.Path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", p.Path)
	}

	if p.Offset > 0 || p.Limit > 0 {
		// Line-based reading
		return readLines(path, p.Offset, p.Limit, info.Size())
	}

	// Read entire file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.Path, err)
	}

	return &protocol.FileReadResultPayload{
		Content: base64.StdEncoding.EncodeToString(data),
		Size:    info.Size(),
	}, nil
}

func readLines(path string, offset, limit int, totalSize int64) (*protocol.FileReadResultPayload, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB lines

	var lines []byte
	lineNum := 0
	collected := 0

	for scanner.Scan() {
		lineNum++
		if offset > 0 && lineNum < offset {
			continue
		}
		if limit > 0 && collected >= limit {
			break
		}
		if collected > 0 {
			lines = append(lines, '\n')
		}
		lines = append(lines, scanner.Bytes()...)
		collected++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return &protocol.FileReadResultPayload{
		Content: base64.StdEncoding.EncodeToString(lines),
		Size:    totalSize,
	}, nil
}
