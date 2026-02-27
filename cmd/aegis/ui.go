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
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	uiFS "github.com/xfeldman/aegisvm/ui"
)

func cmdUI() {
	port := 7700

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &port)
				i++
			}
		case "--help", "-h":
			fmt.Println(`Usage: aegis ui [--port PORT]

Starts the Aegis web UI.

Options:
  --port PORT   HTTP listen port (default: 7700)`)
			return
		}
	}

	// Ensure aegisd is running
	if !isDaemonRunning() {
		log.Println("aegis ui: aegisd not running, starting...")
		startDaemon()
	}

	mux := http.NewServeMux()

	// API proxy: /api/v1/* → aegisd unix socket
	mux.Handle("/api/", newAegisdProxy())

	// Serve embedded frontend
	distFS, err := fs.Sub(uiFS.Frontend, "frontend/dist")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: embedded frontend not found (run 'make ui' first)\n")
		os.Exit(1)
	}
	mux.Handle("/", spaHandler(http.FileServer(http.FS(distFS)), distFS))

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("aegis ui: http://%s", addr)

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
		f, err := fsys.Open(path)
		if err != nil {
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
		ResponseHeaderTimeout: 30 * time.Second,
	}

	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = "aegis"
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
	}

	return &httputil.ReverseProxy{
		Director:      director,
		Transport:     transport,
		FlushInterval: -1, // flush immediately for streaming (NDJSON, logs)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":"aegisd not running"}`)
		},
	}
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
