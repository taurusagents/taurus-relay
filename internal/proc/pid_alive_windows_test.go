//go:build windows

package proc

import "golang.org/x/sys/windows"

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	// Windows lacks syscall.Kill(pid, 0), so test-only liveness probes open the
	// process with query rights instead. If the process is gone, OpenProcess fails.
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(handle)
	return true
}
