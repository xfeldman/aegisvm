package harness

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// countEstablishedTCP returns the number of ESTABLISHED TCP connections
// in the guest by parsing /proc/net/tcp and /proc/net/tcp6.
// State 01 = ESTABLISHED in the kernel's hex encoding.
func countEstablishedTCP() int {
	count := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			// Column 3 (0-indexed) is the connection state
			if fields[3] == "01" {
				count++
			}
		}
	}
	return count
}

// processUsedCPUTicks returns total CPU ticks used by the entire VM.
// Since this is a single-purpose microVM, we use system-wide CPU from /proc/stat
// rather than per-process stats. This correctly captures child processes
// (bash tool calls, git clone, pip install, etc.) that would otherwise be missed.
// Falls back to per-process stats if /proc/stat is unavailable.
func processUsedCPUTicks(pid int) int64 {
	// Try system-wide CPU first — covers all processes in the VM
	data, err := os.ReadFile("/proc/stat")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "cpu ") {
				fields := strings.Fields(line)
				// cpu user nice system idle iowait irq softirq steal
				// Sum user + nice + system (fields 1-3) for active CPU
				if len(fields) >= 4 {
					var total int64
					for _, f := range fields[1:4] {
						v, _ := strconv.ParseInt(f, 10, 64)
						total += v
					}
					return total
				}
			}
		}
	}

	// Fallback: per-process stats
	data, err = os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	closeParen := strings.LastIndex(s, ")")
	if closeParen < 0 || closeParen+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[closeParen+2:])
	if len(fields) < 13 {
		return 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	return utime + stime
}

// ethByteCounters returns the total tx and rx bytes for eth0.
// Returns (0, 0) if the interface doesn't exist or sysfs is unavailable.
// Caller should compute delta between samples.
func ethByteCounters() (tx, rx int64) {
	txData, err := os.ReadFile("/sys/class/net/eth0/statistics/tx_bytes")
	if err != nil {
		return 0, 0
	}
	rxData, err := os.ReadFile("/sys/class/net/eth0/statistics/rx_bytes")
	if err != nil {
		return 0, 0
	}
	tx, _ = strconv.ParseInt(strings.TrimSpace(string(txData)), 10, 64)
	rx, _ = strconv.ParseInt(strings.TrimSpace(string(rxData)), 10, 64)
	return tx, rx
}
