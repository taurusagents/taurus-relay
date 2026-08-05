package fileops

import (
	"fmt"
	"os"
)

// Windows has no unix uid/gid, and node mode (the only caller) is unsupported
// there, so this never runs in practice.
func pathOwner(path string) (int, int, error) {
	return 0, 0, fmt.Errorf("unix ownership of %s is not available on Windows", path)
}

func fileOwner(info os.FileInfo) (int, int, error) {
	return 0, 0, fmt.Errorf("unix ownership of %s is not available on Windows", info.Name())
}
