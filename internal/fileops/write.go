package fileops

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Write writes content to a file, creating parent directories as needed.
func Write(p *protocol.FileWritePayload) (*protocol.FileWriteResultPayload, error) {
	path, err := ValidatePath(p.Path)
	if err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(p.Content)
	if err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}

	// Create parent directories
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}

	mode := os.FileMode(0644)
	if p.Mode != 0 {
		mode = os.FileMode(p.Mode)
	}

	if err := os.WriteFile(path, data, mode); err != nil {
		return nil, fmt.Errorf("write %s: %w", p.Path, err)
	}

	return &protocol.FileWriteResultPayload{
		BytesWritten: len(data),
	}, nil
}

// Mkdir creates a directory.
func Mkdir(p *protocol.FileMkdirPayload) error {
	path, err := ValidatePath(p.Path)
	if err != nil {
		return err
	}
	if p.Recursive {
		return os.MkdirAll(path, 0755)
	}
	return os.Mkdir(path, 0755)
}

// Remove removes a file or directory.
func Remove(p *protocol.FileRemovePayload) error {
	path, err := ValidatePath(p.Path)
	if err != nil {
		return err
	}
	if p.Recursive {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// Stat returns file information.
func Stat(p *protocol.FileStatPayload) (*protocol.FileStatResultPayload, error) {
	path, err := ValidatePath(p.Path)
	if err != nil {
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
