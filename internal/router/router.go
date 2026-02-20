// Package router provides ingress for instances.
//
// Two ingress paths:
//   - Main HTTP router (:8099) for handle-based routing with HTTP reverse proxy
//   - Per-port TCP proxies for --expose ports with L4 relay and wake-on-connect
//
// Both paths share the same "ensure instance + dial backend + relay" core.
// Public ports (router-owned) are stable across pause/resume/stop/restart.
// They are freed only on instance delete or daemon shutdown.
package router

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xfeldman/aegisvm/internal/lifecycle"
)

// PublicEndpoint describes a router-owned public port mapping.
type PublicEndpoint struct {
	GuestPort  int
	PublicPort int
	Protocol   string
}

// portProxy represents a single per-port TCP listener owned by the router.
type portProxy struct {
	instanceID string
	guestPort  int
	publicPort int
	protocol   string
	listener   net.Listener
	cancel     context.CancelFunc
}

// Router is the ingress proxy for instances.
type Router struct {
	lm          *lifecycle.Manager
	addr        string
	server      *http.Server
	mu          sync.Mutex
	portProxies map[string][]*portProxy // instanceID → proxies
}

// New creates a new router.
func New(lm *lifecycle.Manager, addr string) *Router {
	r := &Router{
		lm:          lm,
		addr:        addr,
		portProxies: make(map[string][]*portProxy),
	}
	r.server = &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(r.handleRequest),
	}
	return r
}

// Start begins listening and serving the main HTTP router.
func (r *Router) Start() error {
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		return fmt.Errorf("router listen on %s: %w", r.addr, err)
	}

	log.Printf("router listening on %s", r.addr)

	go func() {
		if err := r.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("router error: %v", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the router and all port proxies.
func (r *Router) Stop(ctx context.Context) error {
	// Close all port proxy listeners
	r.mu.Lock()
	allIDs := make([]string, 0, len(r.portProxies))
	for id := range r.portProxies {
		allIDs = append(allIDs, id)
	}
	r.mu.Unlock()
	for _, id := range allIDs {
		r.FreeAllPorts(id)
	}

	return r.server.Shutdown(ctx)
}

// Addr returns the listen address.
func (r *Router) Addr() string {
	return r.addr
}

// --- Per-port proxy management ---

// AllocatePort creates a public TCP listener for a guest port.
// If requestedPort is 0, a random port is allocated.
// If requestedPort is non-zero, that specific port is used.
// Returns the allocated host port.
func (r *Router) AllocatePort(instanceID string, guestPort int, requestedPort int, protocol string) (int, error) {
	listenAddr := "127.0.0.1:0"
	if requestedPort > 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", requestedPort)
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return 0, fmt.Errorf("allocate public port for guest %d: %w", guestPort, err)
	}
	publicPort := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	pp := &portProxy{
		instanceID: instanceID,
		guestPort:  guestPort,
		publicPort: publicPort,
		protocol:   protocol,
		listener:   ln,
		cancel:     cancel,
	}

	r.mu.Lock()
	r.portProxies[instanceID] = append(r.portProxies[instanceID], pp)
	r.mu.Unlock()

	go r.acceptLoop(ctx, pp)

	log.Printf("router: public port :%d → instance %s guest :%d (%s)",
		publicPort, instanceID, guestPort, protocol)
	return publicPort, nil
}

// FreeAllPorts closes all public port listeners for an instance.
// Called on instance delete (NOT on stop/pause — listeners survive state transitions).
func (r *Router) FreeAllPorts(instanceID string) {
	r.mu.Lock()
	proxies, ok := r.portProxies[instanceID]
	if ok {
		delete(r.portProxies, instanceID)
	}
	r.mu.Unlock()

	if !ok {
		return
	}

	for _, pp := range proxies {
		pp.cancel()
		pp.listener.Close()
		log.Printf("router: freed public port :%d for instance %s", pp.publicPort, instanceID)
	}
}

// GetPublicPort returns the public host port for a guest port.
func (r *Router) GetPublicPort(instanceID string, guestPort int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pp := range r.portProxies[instanceID] {
		if pp.guestPort == guestPort {
			return pp.publicPort, nil
		}
	}
	return 0, fmt.Errorf("no public port for instance %s guest port %d", instanceID, guestPort)
}

// GetAllPublicPorts returns all public endpoints for an instance.
func (r *Router) GetAllPublicPorts(instanceID string) []PublicEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	proxies := r.portProxies[instanceID]
	eps := make([]PublicEndpoint, len(proxies))
	for i, pp := range proxies {
		eps[i] = PublicEndpoint{
			GuestPort:  pp.guestPort,
			PublicPort: pp.publicPort,
			Protocol:   pp.protocol,
		}
	}
	return eps
}

// --- Per-port TCP proxy ---

func (r *Router) acceptLoop(ctx context.Context, pp *portProxy) {
	for {
		conn, err := pp.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return // clean shutdown
			default:
				// Listener closed
				return
			}
		}
		go r.handlePortConn(ctx, conn, pp)
	}
}

func (r *Router) handlePortConn(ctx context.Context, clientConn net.Conn, pp *portProxy) {
	defer clientConn.Close()

	// Track connection BEFORE ensure to prevent idle timer race
	r.lm.ResetActivity(pp.instanceID)
	defer r.lm.OnConnectionClose(pp.instanceID)

	// Ensure instance is running (wake/boot)
	ensureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := r.lm.EnsureInstance(ensureCtx, pp.instanceID); err != nil {
		log.Printf("router: port :%d ensure instance %s: %v", pp.publicPort, pp.instanceID, err)
		return
	}

	// Get VMM backend endpoint
	backend, err := r.lm.GetEndpoint(pp.instanceID, pp.guestPort)
	if err != nil {
		log.Printf("router: port :%d no backend for guest :%d: %v", pp.publicPort, pp.guestPort, err)
		return
	}

	// L4 TCP relay to backend
	r.relay(clientConn, backend)
}

// relay does bidirectional TCP copy between client and backend.
func (r *Router) relay(clientConn net.Conn, backend string) {
	backendConn, err := net.DialTimeout("tcp", backend, 5*time.Second)
	if err != nil {
		log.Printf("router: dial backend %s: %v", backend, err)
		return
	}
	defer backendConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, backendConn)
		done <- struct{}{}
	}()
	<-done
}

// --- Main HTTP router (handle-based routing on :8099) ---

func (r *Router) handleRequest(w http.ResponseWriter, req *http.Request) {
	inst := r.resolveInstance(req)
	if inst == nil {
		http.Error(w, "No active instance", http.StatusServiceUnavailable)
		return
	}

	// Track connection BEFORE ensure to prevent idle timer race
	r.lm.ResetActivity(inst.ID)
	defer r.lm.OnConnectionClose(inst.ID)

	// Ensure instance is running (wake if paused, boot if stopped)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	if err := r.lm.EnsureInstance(ctx, inst.ID); err != nil {
		log.Printf("router: ensure instance %s: %v", inst.ID, err)
		w.Header().Set("Retry-After", "3")
		http.Error(w, fmt.Sprintf("Service unavailable: %v", err), http.StatusServiceUnavailable)
		return
	}

	// Get the host endpoint for the first exposed port
	guestPort := inst.FirstGuestPort()
	if guestPort == 0 {
		http.Error(w, "No exposed ports", http.StatusServiceUnavailable)
		return
	}

	target, err := r.lm.GetEndpoint(inst.ID, guestPort)
	if err != nil {
		http.Error(w, fmt.Sprintf("endpoint not found: %v", err), http.StatusBadGateway)
		return
	}

	// Check for WebSocket upgrade
	if isWebSocketUpgrade(req) {
		r.handleWebSocket(w, req, target)
		return
	}

	// Standard HTTP reverse proxy
	targetURL, _ := url.Parse("http://" + target)
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = targetURL.Scheme
			r.URL.Host = targetURL.Host
			r.Host = targetURL.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("router proxy error: %v", err)
			w.Header().Set("Retry-After", "3")
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		},
	}

	proxy.ServeHTTP(w, req)
}

func (r *Router) handleWebSocket(w http.ResponseWriter, req *http.Request, target string) {
	// Dial backend
	backendConn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		http.Error(w, "WebSocket backend connection failed", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Hijack the client connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "WebSocket hijack failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Forward the original upgrade request to the backend
	if err := req.Write(backendConn); err != nil {
		return
	}

	// Flush any buffered data from the client
	if clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		clientBuf.Read(buffered)
		backendConn.Write(buffered)
	}

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, backendConn)
		done <- struct{}{}
	}()
	<-done
}

// resolveInstance finds an instance for a request.
// Resolution order:
//  1. X-Aegis-Instance header → route to instance by ID or handle
//  2. Fall back to default instance if only one exists
func (r *Router) resolveInstance(req *http.Request) *lifecycle.Instance {
	// 1. Header-based routing: X-Aegis-Instance (ID or handle)
	if instRef := req.Header.Get("X-Aegis-Instance"); instRef != "" {
		if inst := r.lm.GetInstance(instRef); inst != nil {
			return inst
		}
		return r.lm.GetInstanceByHandle(instRef)
	}

	// 2. Default instance fallback
	count := r.lm.InstanceCount()
	if count == 1 {
		return r.lm.GetDefaultInstance()
	}
	if count > 1 {
		log.Printf("router: ambiguous request to %s with %d instances — use X-Aegis-Instance header or per-port endpoints", req.URL.Path, count)
	}
	return nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
