//go:build linux

package health

func totalPhysicalMemoryBytes() uint64 {
	totalKB, _ := memoryTotalsLinux()
	return totalKB * 1024
}
