//go:build !windows

package fileops

import "syscall"

// oNoFollow makes open(2) refuse to traverse a symlink at the final path
// component, so an agent cannot redirect a relay-side create through a symlink
// it planted in its own drive.
const oNoFollow = syscall.O_NOFOLLOW
