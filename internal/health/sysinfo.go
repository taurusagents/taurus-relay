// Package health provides system information and heartbeat functionality.
package health

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Version is set at build time via -ldflags.
var Version = "dev"

var startTime = time.Now()

// SysInfo gathers current system information for relay heartbeats.
func SysInfo(sessionCount int) *protocol.HeartbeatPayload {
	hostname, _ := os.Hostname()
	return &protocol.HeartbeatPayload{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Hostname:      hostname,
		Uptime:        int64(time.Since(startTime).Seconds()),
		CPUPercent:    0, // TODO: implement with runtime metrics
		MemoryPercent: memoryPercent(),
		RelayVersion:  Version,
		Sessions:      sessionCount,
	}
}

// NodeSysInfo gathers node-specific heartbeat info used for scheduling.
func NodeSysInfo(sessionCount int, dataPath string, containerCount int) *protocol.HeartbeatPayload {
	base := SysInfo(sessionCount)
	usedGB, availableGB := memoryUsageGB()
	base.ContainerCount = containerCount
	base.MemoryUsedGB = usedGB
	base.MemoryAvailableGB = availableGB
	base.CPULoad = cpuLoad()
	base.DiskUsedGB, base.DiskAvailableGB = diskUsageGB(dataPath)
	return base
}

// NodeAllocatable returns total system memory (GB) and logical CPU count.
func NodeAllocatable() (float64, int) {
	totalKB, _ := memoryTotalsKB()
	return float64(totalKB) / (1024 * 1024), runtime.NumCPU()
}

func memoryUsageGB() (usedGB, availableGB float64) {
	totalKB, availableKB := memoryTotalsKB()
	if totalKB == 0 {
		return 0, 0
	}
	usedKB := totalKB - availableKB
	return float64(usedKB) / (1024 * 1024), float64(availableKB) / (1024 * 1024)
}

func memoryTotalsKB() (totalKB, availableKB uint64) {
	switch runtime.GOOS {
	case "linux":
		return memoryTotalsLinux()
	default:
		return 0, 0
	}
}

// memoryPercent returns system memory usage percentage.
// Uses /proc/meminfo on Linux, sysctl on macOS. Returns 0 if unavailable.
func memoryPercent() float64 {
	switch runtime.GOOS {
	case "linux":
		totalKB, availableKB := memoryTotalsLinux()
		if totalKB == 0 {
			return 0
		}
		used := totalKB - availableKB
		return float64(used) / float64(totalKB) * 100
	case "darwin":
		return memoryPercentDarwin()
	default:
		return 0
	}
}

func memoryTotalsLinux() (totalKB, availableKB uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			totalKB = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			availableKB = parseMemInfoValue(line)
		}
		if totalKB > 0 && availableKB > 0 {
			break
		}
	}
	return totalKB, availableKB
}

func cpuLoad() float64 {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			parts := strings.Fields(string(data))
			if len(parts) > 0 {
				if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// parseMemInfoValue extracts the kB value from a /proc/meminfo line like "MemTotal:  16384000 kB"
func parseMemInfoValue(line string) uint64 {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	val, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return val
}

func memoryPercentDarwin() float64 {
	// Get total physical memory via sysctl
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	totalBytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || totalBytes == 0 {
		return 0
	}

	// Get page size and vm_stat for used memory
	pageOut, err := exec.Command("pagesize").Output()
	if err != nil {
		return 0
	}
	pageSize, err := strconv.ParseUint(strings.TrimSpace(string(pageOut)), 10, 64)
	if err != nil || pageSize == 0 {
		return 0
	}

	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}

	// Parse active + wired pages from vm_stat output
	var activePages, wiredPages uint64
	scanner := bufio.NewScanner(strings.NewReader(string(vmOut)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Pages active:") {
			activePages = parseVMStatValue(line)
		} else if strings.HasPrefix(line, "Pages wired down:") {
			wiredPages = parseVMStatValue(line)
		}
	}

	usedBytes := (activePages + wiredPages) * pageSize
	return float64(usedBytes) / float64(totalBytes) * 100
}

// parseVMStatValue extracts the page count from a vm_stat line like "Pages active:   123456."
func parseVMStatValue(line string) uint64 {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return 0
	}
	valStr := strings.TrimSuffix(parts[len(parts)-1], ".")
	val, err := strconv.ParseUint(valStr, 10, 64)
	if err != nil {
		return 0
	}
	return val
}
