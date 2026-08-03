package fileops

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from -> to, failing if the destination already exists.
//
// RENAME_NOREPLACE makes that atomic. It needs Linux >= 3.15 and filesystem
// support (ext4/XFS/btrfs/tmpfs have it); where the kernel or the filesystem
// says no, fall back to an explicit existence check, which is what a
// check-then-act `mv -n` would have done anyway.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return destinationExistsError(to)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return renameNoReplaceFallback(from, to)
	default:
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
}

func renameNoReplaceFallback(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return destinationExistsError(to)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}
