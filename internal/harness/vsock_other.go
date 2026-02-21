//go:build !linux

package harness

import (
	"fmt"
	"net"
)

// dialVsock is not supported on non-Linux platforms.
// The harness only runs inside Linux VMs.
func dialVsock(portStr, cidStr string) (net.Conn, error) {
	return nil, fmt.Errorf("vsock not supported on this platform")
}
