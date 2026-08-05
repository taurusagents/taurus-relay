//go:build !linux

package fileops

import "os"

// Non-Linux hosts have no renameat2, so the no-clobber guarantee degrades to a
// check-then-act. Node mode (the only caller) is Linux in practice.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return destinationExistsError(to)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}
