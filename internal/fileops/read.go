// Package fileops implements structured file operations.
package fileops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

const fileReadChunkSize = 64 * 1024

// Read reads a file and returns its content as base64.
// offset and limit are in lines (1-based), 0 means no limit.
func Read(p *protocol.FileReadPayload) (*protocol.FileReadResultPayload, error) {
	return ReadContext(context.Background(), p)
}

// ReadContext does the same work as Read but lets runtime reset cancel long-running scans.
func ReadContext(ctx context.Context, p *protocol.FileReadPayload) (*protocol.FileReadResultPayload, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	path, err := ValidatePath(p.Path)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
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
		// Line-based reading checks the runtime context between scanned lines so a
		// reset can stop a stale request before it walks the whole file.
		return readLinesContext(ctx, path, p.Offset, p.Limit, info.Size())
	}

	return readAllContext(ctx, path, info.Size())
}

func readAllContext(ctx context.Context, path string, totalSize int64) (*protocol.FileReadResultPayload, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	var data bytes.Buffer
	buf := make([]byte, fileReadChunkSize)
	for {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = data.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	return &protocol.FileReadResultPayload{
		Content: base64.StdEncoding.EncodeToString(data.Bytes()),
		Size:    totalSize,
	}, nil
}

func readLinesContext(ctx context.Context, path string, offset, limit int, totalSize int64) (*protocol.FileReadResultPayload, error) {
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
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
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
