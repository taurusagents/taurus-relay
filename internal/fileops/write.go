package fileops

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

const fileWriteChunkSize = 64 * 1024

// Write writes content to a file, creating parent directories as needed.
func Write(p *protocol.FileWritePayload) (*protocol.FileWriteResultPayload, error) {
	return WriteContext(context.Background(), p)
}

// WriteContext stages the write through a temp file so a reset can cancel stale
// work before it mutates the target path.
func WriteContext(ctx context.Context, p *protocol.FileWritePayload) (*protocol.FileWriteResultPayload, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	path, err := ValidatePath(p.Path)
	if err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(p.Content)
	if err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	// Create parent directories before we open the temp file.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}

	mode := os.FileMode(0o644)
	if p.Mode != 0 {
		mode = os.FileMode(p.Mode)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	for start := 0; start < len(data); start += fileWriteChunkSize {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		end := start + fileWriteChunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := tmp.Write(data[start:end]); err != nil {
			return nil, fmt.Errorf("write temp file: %w", err)
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return nil, fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("write %s: %w", p.Path, err)
	}
	tmpPath = ""

	return &protocol.FileWriteResultPayload{
		BytesWritten: len(data),
	}, nil
}

// Mkdir creates a directory.
func Mkdir(p *protocol.FileMkdirPayload) error {
	return MkdirContext(context.Background(), p)
}

func MkdirContext(ctx context.Context, p *protocol.FileMkdirPayload) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	path, err := ValidatePath(p.Path)
	if err != nil {
		return err
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if p.Recursive {
		return os.MkdirAll(path, 0o755)
	}
	return os.Mkdir(path, 0o755)
}

// Remove removes a file or directory.
func Remove(p *protocol.FileRemovePayload) error {
	return RemoveContext(context.Background(), p)
}

func RemoveContext(ctx context.Context, p *protocol.FileRemovePayload) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	path, err := ValidatePath(p.Path)
	if err != nil {
		return err
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if p.Recursive {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// Stat returns file information.
func Stat(p *protocol.FileStatPayload) (*protocol.FileStatResultPayload, error) {
	return StatContext(context.Background(), p)
}

func StatContext(ctx context.Context, p *protocol.FileStatPayload) (*protocol.FileStatResultPayload, error) {
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
		return nil, err
	}

	return &protocol.FileStatResultPayload{
		Size:  info.Size(),
		Mode:  uint32(info.Mode()),
		Mtime: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		IsDir: info.IsDir(),
	}, nil
}
