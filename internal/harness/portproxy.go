package harness

import (
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// portProxy forwards TCP connections from guestIP:port to localhost:port.
//
// With gvproxy networking, inbound traffic arrives at the guest's eth0 IP
// (e.g. 192.168.127.2). Apps that bind to 127.0.0.1 won't receive this
// traffic. The port proxy bridges the gap: it listens on guestIP:port and
// forwards to 127.0.0.1:port, making localhost-bound apps reachable.
//
// The proxy binds to the specific guest IP (not 0.0.0.0) to avoid conflicts
// with apps that bind to 127.0.0.1 on the same port.
//
// When the app binds to 0.0.0.0 itself, the proxy's listen will fail with
// EADDRINUSE and be skipped — the app handles the traffic directly.
type portProxy struct {
	mu        sync.Mutex
	listeners []net.Listener
}

// startPortProxies starts a TCP proxy for each exposed guest port after a delay.
// Each proxy listens on guestIP:port and forwards to 127.0.0.1:port.
// The guest IP is read from AEGIS_NET_IP (e.g. "192.168.127.2/24").
//
// Retries binding periodically. On each attempt:
//   - If guestIP:port binds → proxy runs (app is on localhost, proxy bridges)
//   - If EADDRINUSE → app has 0.0.0.0, no proxy needed, stop retrying
//
// Returns nil if not in gvproxy mode (no AEGIS_NET_IP set).
func startPortProxies(ports []int) *portProxy {
	guestIP := guestIPFromEnv()
	if guestIP == "" {
		return nil // TSI mode, no proxy needed
	}

	pp := &portProxy{}

	for _, port := range ports {
		go pp.tryBind(guestIP, port)
	}

	return pp
}

// tryBind attempts to bind the proxy for a single port, retrying periodically.
// Gives the app time to start (e.g., after pip install) before claiming the port.
// Stops retrying after 5 minutes or when the app takes the wildcard address.
func (pp *portProxy) tryBind(guestIP string, port int) {
	listenAddr := net.JoinHostPort(guestIP, strconv.Itoa(port))

	for attempt := 0; attempt < 30; attempt++ {
		// Wait before each attempt (2s, then every 10s)
		if attempt == 0 {
			time.Sleep(2 * time.Second)
		} else {
			time.Sleep(10 * time.Second)
		}

		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			// Check if the app bound to 0.0.0.0 (no proxy needed)
			// vs some transient error worth retrying
			testConn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
			if dialErr == nil {
				testConn.Close()
				// App is listening on localhost — but we couldn't bind guestIP.
				// This means app took 0.0.0.0 (wildcard). No proxy needed.
				log.Printf("portproxy: skip %d (app bound to wildcard)", port)
				return
			}
			// Nobody listening yet — keep retrying
			continue
		}

		pp.mu.Lock()
		pp.listeners = append(pp.listeners, ln)
		pp.mu.Unlock()

		log.Printf("portproxy: %s → 127.0.0.1:%d", listenAddr, port)
		go pp.accept(ln, port)
		return
	}

	log.Printf("portproxy: gave up on port %d after retries", port)
}

// guestIPFromEnv extracts the IP address (without prefix length) from AEGIS_NET_IP.
// Returns "" if not set (TSI mode).
func guestIPFromEnv() string {
	cidr := os.Getenv("AEGIS_NET_IP")
	if cidr == "" {
		return ""
	}
	// Strip /prefix (e.g. "192.168.127.2/24" → "192.168.127.2")
	if idx := strings.IndexByte(cidr, '/'); idx >= 0 {
		return cidr[:idx]
	}
	return cidr
}

func (pp *portProxy) accept(ln net.Listener, port int) {
	dst := "127.0.0.1:" + strconv.Itoa(port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go pp.relay(conn, dst)
	}
}

func (pp *portProxy) relay(src net.Conn, dst string) {
	defer src.Close()

	backend, err := net.Dial("tcp", dst)
	if err != nil {
		return // app not listening yet or refused
	}
	defer backend.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, src); done <- struct{}{} }()
	go func() { io.Copy(src, backend); done <- struct{}{} }()
	<-done
}

// Stop closes all proxy listeners.
func (pp *portProxy) Stop() {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	for _, ln := range pp.listeners {
		ln.Close()
	}
	pp.listeners = nil
}
