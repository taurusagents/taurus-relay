//go:build !linux && !darwin && !windows

package health

func totalPhysicalMemoryBytes() uint64 {
	return 0
}
