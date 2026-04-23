//go:build linux || darwin

package health

import "syscall"

func diskUsageGB(path string) (usedGB, availableGB float64) {
	if path == "" {
		path = "/"
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}

	total := float64(st.Blocks) * float64(st.Bsize)
	available := float64(st.Bavail) * float64(st.Bsize)
	used := total - (float64(st.Bfree) * float64(st.Bsize))
	return used / (1024 * 1024 * 1024), available / (1024 * 1024 * 1024)
}
