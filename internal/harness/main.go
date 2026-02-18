// Package harness implements the guest-side agent harness that runs as PID 1
// inside Aegis microVMs.
//
// The harness connects outbound to the host's RPC listener via TSI (Transparent
// Socket Impersonation) and handles JSON-RPC 2.0 commands. TSI transparently
// routes AF_INET connections from the guest through vsock to the host.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o aegis-harness ./internal/harness
package harness

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Run starts the harness. This is the main entry point called by the harness binary.
func Run() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("aegis-harness starting")

	// Mount /proc and /tmp if not already mounted (we are PID 1)
	mountEssential()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)
		cancel()
	}()

	// Get the host RPC address from env (set by aegisd via vmm-worker).
	// In libkrun's standard mode, TSI intercepts outbound AF_INET connections
	// and routes them through vsock to the host, so 127.0.0.1:PORT reaches
	// the host's actual localhost.
	hostAddr := os.Getenv("AEGIS_HOST_ADDR")
	if hostAddr == "" {
		log.Fatal("AEGIS_HOST_ADDR not set — cannot connect to host")
	}

	log.Printf("connecting to host at %s", hostAddr)

	// Connect to host with retry (host listener may not be ready yet)
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("tcp", hostAddr, 2*time.Second)
		if err == nil {
			break
		}
		log.Printf("connect attempt %d failed: %v", i+1, err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		log.Fatalf("failed to connect to host after retries: %v", err)
	}
	defer conn.Close()

	log.Printf("connected to host at %s", hostAddr)

	// Handle JSON-RPC commands from the host over this connection
	handleConnection(ctx, conn)

	log.Println("harness shutting down")
}

// mountEssential mounts /proc and /tmp if they are not already mounted.
func mountEssential() {
	mounts := []struct {
		source string
		target string
		fstype string
		flags  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"tmpfs", "/tmp", "tmpfs", 0},
		{"tmpfs", "/run", "tmpfs", 0},
	}

	for _, m := range mounts {
		_ = os.MkdirAll(m.target, 0755)
		err := syscall.Mount(m.source, m.target, m.fstype, m.flags, "")
		if err != nil && err != syscall.EBUSY {
			log.Printf("mount %s on %s: %v (non-fatal)", m.source, m.target, err)
		}
	}
}
