//go:build !linux

package harness

func countEstablishedTCP() int           { return 0 }
func processUsedCPUTicks(pid int) int64  { return 0 }
func ethByteCounters() (tx, rx int64)    { return 0, 0 }
