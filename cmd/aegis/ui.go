package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	uiFS "github.com/xfeldman/aegisvm/ui"
)

func cmdUI() {
	port := 7700
	devMode := false

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &port)
				i++
			}
		case "--dev":
			devMode = true
		case "--help", "-h":
			fmt.Println(`Usage: aegis ui [--port PORT] [--dev]

Starts the Aegis web UI.

Options:
  --port PORT   HTTP listen port (default: 7700)
  --dev         Proxy frontend to Vite dev server (localhost:5173)`)
			return
		}
	}

	mux := http.NewServeMux()

	// API proxy: /api/v1/* → aegisd unix socket
	mux.Handle("/api/", newAegisdProxy())

	// Frontend
	if devMode {
		// Proxy to Vite dev server for hot reload
		viteURL, _ := url.Parse("http://127.0.0.1:5173")
		viteProxy := httputil.NewSingleHostReverseProxy(viteURL)
		mux.Handle("/", viteProxy)
		log.Printf("aegis ui: dev mode, proxying frontend to %s", viteURL)
	} else {
		// Serve embedded frontend build
		distFS, err := fs.Sub(uiFS.Frontend, "frontend/dist")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: embedded frontend not found (run 'make ui' first)\n")
			os.Exit(1)
		}
		fileServer := http.FileServer(http.FS(distFS))
		mux.Handle("/", spaHandler(fileServer, distFS))
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("aegis ui: http://%s", addr)

	// Open browser
	go func() {
		time.Sleep(200 * time.Millisecond)
		openBrowser("http://" + addr)
	}()

	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// spaHandler serves static files, falling back to index.html for SPA routing.
func spaHandler(fileServer http.Handler, fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// Try to open the requested file
		f, err := fsys.Open(path)
		if err != nil {
			// File not found — serve index.html for SPA routing
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
			return
		}
		f.Close()
		fileServer.ServeHTTP(w, r)
	})
}

// newAegisdProxy creates a reverse proxy that forwards to aegisd via unix socket.
// It strips the /api prefix so /api/v1/instances → /v1/instances.
func newAegisdProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			d.Timeout = 5 * time.Second
			return d.DialContext(ctx, "unix", socketPath())
		},
		// Streaming-friendly: no response buffering limit
		ResponseHeaderTimeout: 30 * time.Second,
	}

	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = "aegis"
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
	}

	proxy := &httputil.ReverseProxy{
		Director:      director,
		Transport:     transport,
		FlushInterval: -1, // flush immediately for streaming (NDJSON, logs)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":"aegisd not running"}`)
		},
	}

	return proxy
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	cmd.Start()
}
