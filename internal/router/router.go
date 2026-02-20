// Package router provides the HTTP reverse proxy that fronts instances.
// It listens on a local port (default 127.0.0.1:8099) and proxies incoming
// requests to the VM's mapped host port. It handles wake-on-connect (resuming
// paused VMs), connection tracking, and WebSocket upgrades.
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

// Router is the HTTP reverse proxy for instances.
type Router struct {
	lm     *lifecycle.Manager
	addr   string
	server *http.Server
	mu     sync.Mutex
}

// New creates a new router.
func New(lm *lifecycle.Manager, addr string) *Router {
	r := &Router{
		lm:   lm,
		addr: addr,
	}
	r.server = &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(r.handleRequest),
	}
	return r
}

// Start begins listening and serving.
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

// Stop gracefully shuts down the router.
func (r *Router) Stop(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}

// Addr returns the listen address.
func (r *Router) Addr() string {
	return r.addr
}

func (r *Router) handleRequest(w http.ResponseWriter, req *http.Request) {
	inst := r.resolveInstance(req)
	if inst == nil {
		http.Error(w, "No active instance", http.StatusServiceUnavailable)
		return
	}

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

	// Track connection
	r.lm.ResetActivity(inst.ID)
	defer r.lm.OnConnectionClose(inst.ID)

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
//  1. X-Aegis-Instance header → route to instance by ID
//  2. Path prefix: /{handle}/... → strip handle, route to instance by handle
//  3. Fall back to default instance if only one exists
func (r *Router) resolveInstance(req *http.Request) *lifecycle.Instance {
	// 1. Header-based routing: X-Aegis-Instance
	if instID := req.Header.Get("X-Aegis-Instance"); instID != "" {
		return r.lm.GetInstance(instID)
	}

	// 2. Path-based routing: /{handle}/...
	if req.URL.Path != "/" && req.URL.Path != "" {
		path := strings.TrimPrefix(req.URL.Path, "/")
		slashIdx := strings.IndexByte(path, '/')
		var handle string
		if slashIdx >= 0 {
			handle = path[:slashIdx]
		} else {
			handle = path
		}

		if handle != "" {
			inst := r.lm.GetInstanceByHandle(handle)
			if inst != nil {
				// Strip the handle prefix
				if slashIdx >= 0 {
					req.URL.Path = path[slashIdx:]
				} else {
					req.URL.Path = "/"
				}
				return inst
			}
		}
	}

	// 3. Default instance fallback
	count := r.lm.InstanceCount()
	if count == 1 {
		return r.lm.GetDefaultInstance()
	}
	if count > 1 {
		log.Printf("router: ambiguous request to %s with %d instances — use /{handle}/... path or X-Aegis-Instance header", req.URL.Path, count)
	}
	return nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
