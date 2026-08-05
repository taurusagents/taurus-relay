//go:build !windows

package fileops

import (
	"fmt"
	"os"
	"syscall"
)

// pathOwner reports the uid/gid of a path without following a final symlink.
func pathOwner(path string) (int, int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot read unix ownership of %s on this platform", path)
	}
	return int(stat.Uid), int(stat.Gid), nil
}

// fileOwner reads uid/gid out of an already-taken FileInfo, so a caller that has
// just lstat'ed a path does not have to race a second stat against it.
func fileOwner(info os.FileInfo) (int, int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot read unix ownership of %s on this platform", info.Name())
	}
	return int(stat.Uid), int(stat.Gid), nil
}
