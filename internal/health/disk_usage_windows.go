//go:build windows

package health

import (
	"os"

	"golang.org/x/sys/windows"
)

func diskUsageGB(path string) (usedGB, availableGB float64) {
	if path == "" {
		drive := os.Getenv("SystemDrive")
		if drive == "" {
			drive = "C:"
		}
		path = drive + `\`
	}

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}

	var freeBytesAvailable uint64
	var totalBytes uint64
	var totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0
	}

	usedBytes := totalBytes - totalFreeBytes
	const gb = 1024 * 1024 * 1024
	return float64(usedBytes) / gb, float64(totalFreeBytes) / gb
}
