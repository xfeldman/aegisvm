// Package harness implements the guest-side agent harness that runs as PID 1
// inside Aegis microVMs.
//
// The harness connects outbound to the host's control channel. Two modes:
//   - gvproxy (AEGIS_VSOCK_PORT set): connect via AF_VSOCK to host unix socket
//   - TSI legacy (AEGIS_HOST_ADDR set): connect via TCP, TSI intercepts AF_INET
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

	// Mount workspace virtiofs if available (best-effort)
	mountWorkspace()

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

	// Connect to host control channel.
	// Two modes: vsock (gvproxy) or TCP (TSI legacy).
	conn := connectToHost()
	defer conn.Close()

	// Create bidirectional RPC client for guest API
	hrpc := newHarnessRPC(conn)

	// Start guest API HTTP server (localhost:7777)
	// Guest processes call this to spawn/manage instances.
	// The portProxy is set on hrpc by handleRun when the primary process starts.
	go startGuestAPIServer(hrpc)

	// Handle JSON-RPC from the host + responses to our guest API calls
	handleConnection(ctx, conn, hrpc)

	log.Println("harness shutting down")
}

// connectToHost establishes the control channel to the host.
// Prefers vsock (AEGIS_VSOCK_PORT) over TCP/TSI (AEGIS_HOST_ADDR).
func connectToHost() net.Conn {
	vsockPort := os.Getenv("AEGIS_VSOCK_PORT")
	vsockCID := os.Getenv("AEGIS_VSOCK_CID")

	if vsockPort != "" {
		log.Printf("connecting to host via vsock (port=%s, cid=%s)", vsockPort, vsockCID)

		var conn net.Conn
		var err error
		for i := 0; i < 30; i++ {
			conn, err = dialVsock(vsockPort, vsockCID)
			if err == nil {
				log.Printf("connected to host via vsock")
				return conn
			}
			log.Printf("vsock connect attempt %d failed: %v", i+1, err)
			time.Sleep(500 * time.Millisecond)
		}
		log.Fatalf("failed to connect to host via vsock after retries: %v", err)
	}

	// Legacy TSI path
	hostAddr := os.Getenv("AEGIS_HOST_ADDR")
	if hostAddr == "" {
		log.Fatal("neither AEGIS_VSOCK_PORT nor AEGIS_HOST_ADDR set — cannot connect to host")
	}

	log.Printf("connecting to host at %s (TSI mode)", hostAddr)

	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("tcp", hostAddr, 2*time.Second)
		if err == nil {
			log.Printf("connected to host at %s", hostAddr)
			return conn
		}
		log.Printf("connect attempt %d failed: %v", i+1, err)
		time.Sleep(500 * time.Millisecond)
	}
	log.Fatalf("failed to connect to host after retries: %v", err)
	return nil // unreachable
}
