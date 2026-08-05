package fileops

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// createStagingFile opens a fresh temp file in dir that will be renamed over the
// real target.
//
// This exists instead of os.CreateTemp for one reason: os.CreateTemp opens with
// O_CREATE|O_EXCL but *without* O_NOFOLLOW. O_EXCL already refuses an existing
// symlink on Linux, but relying on that is a subtlety nobody should have to
// re-derive, and O_NOFOLLOW states the requirement directly: the relay creates a
// real file at a name it chose, never through a link something else planted.
func createStagingFile(dir, baseName string) (*os.File, string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		suffix := make([]byte, 8)
		if _, err := rand.Read(suffix); err != nil {
			return nil, "", err
		}
		name := filepath.Join(dir, "."+baseName+".tmp-"+hex.EncodeToString(suffix))

		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, 0o600)
		if err == nil {
			return f, name, nil
		}
		if os.IsExist(err) {
			continue // astronomically unlikely; just draw another name
		}
		return nil, "", err
	}
	return nil, "", fmt.Errorf("could not create a staging file in %s after 32 attempts", dir)
}
