//go:build !windows

package proc

import "syscall"

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
