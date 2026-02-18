// aegis-harness is the guest PID 1 process that runs inside Aegis microVMs.
//
// It listens for JSON-RPC 2.0 commands over vsock and executes tasks.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o aegis-harness ./cmd/aegis-harness
package main

import "github.com/xfeldman/aegis/internal/harness"

func main() {
	harness.Run()
}
