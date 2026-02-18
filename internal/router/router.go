// Package router provides the HTTP reverse proxy that fronts serve-mode instances.
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

	"github.com/xfeldman/aegis/internal/lifecycle"
)

// AppResolver looks up an app by name or ID. Used for multi-app routing.
type AppResolver interface {
	GetAppByName(name string) (appID string, ok bool)
}

// Router is the HTTP reverse proxy for serve-mode instances.
type Router struct {
	lm       *lifecycle.Manager
	addr     string
	server   *http.Server
	resolver AppResolver
	mu       sync.Mutex
}

// New creates a new router.
func New(lm *lifecycle.Manager, addr string, resolver AppResolver) *Router {
	r := &Router{
		lm:       lm,
		addr:     addr,
		resolver: resolver,
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
	// M2: try app-based routing via X-Aegis-App header or Host
	inst := r.resolveInstance(req)
	if inst == nil {
		// Fall back to default instance (M1 backward compat)
		inst = r.lm.GetDefaultInstance()
	}
	if inst == nil {
		http.Error(w, "No active instance", http.StatusServiceUnavailable)
		return
	}

	// Ensure instance is running (wake if paused, boot if stopped)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	if err := r.lm.EnsureInstance(ctx, inst.ID); err != nil {
		log.Printf("router: ensure instance %s: %v", inst.ID, err)
		r.serveLoadingPage(w, req, err)
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
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, req)
}

func (r *Router) serveLoadingPage(w http.ResponseWriter, req *http.Request, bootErr error) {
	// If client accepts HTML, serve a loading page with meta refresh
	if strings.Contains(req.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <meta http-equiv="refresh" content="3">
  <title>Starting...</title>
  <style>body{font-family:system-ui;display:flex;justify-content:center;align-items:center;height:100vh;margin:0;background:#f5f5f5}
  .c{text-align:center;color:#666}h1{font-size:1.5em}</style>
</head>
<body>
  <div class="c">
    <h1>Starting instance...</h1>
    <p>This page will refresh automatically.</p>
  </div>
</body>
</html>`)
		return
	}

	// Non-HTML clients get 503 with Retry-After
	w.Header().Set("Retry-After", "3")
	http.Error(w, fmt.Sprintf("Instance starting: %v", bootErr), http.StatusServiceUnavailable)
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

// resolveInstance attempts to find an instance for a request using app-aware routing.
func (r *Router) resolveInstance(req *http.Request) *lifecycle.Instance {
	if r.resolver == nil {
		return nil
	}

	// Check X-Aegis-App header
	appName := req.Header.Get("X-Aegis-App")
	if appName == "" {
		return nil
	}

	appID, ok := r.resolver.GetAppByName(appName)
	if !ok {
		return nil
	}

	return r.lm.GetInstanceByApp(appID)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
