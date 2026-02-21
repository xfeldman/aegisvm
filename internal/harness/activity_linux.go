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

// processUsedCPUTicks returns the total CPU ticks (utime + stime) for a process.
// Returns 0 if the process doesn't exist or /proc is unavailable.
// Caller should compute delta between samples to get actual CPU usage.
func processUsedCPUTicks(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// /proc/pid/stat format: pid (comm) state ... utime stime ...
	// utime is field 14 (1-indexed), stime is field 15.
	// Find the closing paren to skip the comm field (which can contain spaces).
	s := string(data)
	closeParen := strings.LastIndex(s, ")")
	if closeParen < 0 || closeParen+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[closeParen+2:])
	// After ")", fields[0]=state, fields[1]=ppid, ..., fields[11]=utime, fields[12]=stime
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
