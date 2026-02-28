//go:build uifrontend

// aegis-ui is the native desktop app for AegisVM.
//
// It wraps the same web frontend used by "aegis ui" in a native webview
// via Wails v3, adding a system tray and daemon lifecycle management.
//
// Architecture: a local HTTP server handles all requests (API proxy + SPA),
// and the Wails webview points to http://localhost:PORT. This avoids the
// WebKit WKURLSchemeHandler limitation where POST request bodies are dropped
// for custom URL schemes (wails://).
package main

import (
	"context"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os/exec"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	uiFS "github.com/xfeldman/aegisvm/ui"
)

func main() {
	// Extract bundled binaries from .app bundle (macOS) on first launch
	// or version mismatch. No-op in dev mode (binaries next to executable).
	ensureBinaries()

	// Start aegisd if not running.
	ensureDaemon()

	// Extract embedded frontend (same FS used by "aegis ui").
	distFS, err := fs.Sub(uiFS.Frontend, "frontend/dist")
	if err != nil {
		log.Fatalf("embedded frontend not found (run 'make ui-frontend' first): %v", err)
	}

	// Serve via real HTTP. This avoids the WebKit WKURLSchemeHandler
	// limitation where POST bodies are dropped for custom URL schemes.
	// Chat history lives in tether (server-side), so any port works.
	mux := http.NewServeMux()
	mux.Handle("/api/", newAegisdProxy())
	mux.HandleFunc("POST /open-url", handleOpenURL)
	mux.Handle("/", spaHandler(http.FileServer(http.FS(distFS)), distFS))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("aegis-ui: listen: %v", err)
	}
	uiAddr := listener.Addr().String()
	go http.Serve(listener, mux)

	app := application.New(application.Options{
		Name: "AegisVM",
	})

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "AegisVM",
		URL:    "http://" + uiAddr,
		Width:  1100,
		Height: 700,
		// JS injected into the webview to fix WKWebView limitations:
		// 1. window.confirm() — WKWebView silently returns false without a WKUIDelegate handler.
		//    Replace with a synchronous prompt using a blocking overlay + buttons.
		// 2. target="_blank" links — WKWebView ignores them without createWebView delegate.
		//    Intercept clicks and open in the system browser via a local endpoint.
		JS: desktopJS,
		Mac: application.MacWindow{
			Backdrop:                application.MacBackdropNormal,
			TitleBar:                application.MacTitleBarHidden,
			InvisibleTitleBarHeight: 38, // matches .topbar height — drag zone
		},
	})

	setupSystemTray(app, window)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// spaHandler serves static files, falling back to index.html for SPA routing.
// Unlike the CLI's handler, this injects class="desktop-app" on <html> so the
// frontend can apply desktop-only styles (compact toolbar, traffic-light padding).
func spaHandler(fileServer http.Handler, fsys fs.FS) http.Handler {
	// Read and patch index.html once at startup.
	indexData, _ := fs.ReadFile(fsys, "index.html")
	indexHTML := strings.Replace(string(indexData), "<html", `<html class="desktop-app"`, 1)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(indexHTML))
			return
		}
		f, err := fsys.Open(path)
		if err != nil {
			// SPA fallback: serve patched index.html for unknown paths
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(indexHTML))
			return
		}
		f.Close()
		fileServer.ServeHTTP(w, r)
	})
}

// newAegisdProxy creates a reverse proxy that forwards /api/* to aegisd
// via the unix socket. Strips the /api prefix so /api/v1/instances → /v1/instances.
// Identical to the proxy in cmd/aegis/ui.go.
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
			w.Write([]byte(`{"error":"aegisd not running"}`))
		},
	}
}

// handleOpenURL opens a URL in the system browser.
func handleOpenURL(w http.ResponseWriter, r *http.Request) {
	url := r.FormValue("url")
	if url == "" || (!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://")) {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	exec.Command("open", url).Start()
	w.WriteHeader(http.StatusNoContent)
}

// desktopJS opens target="_blank" links in the system browser.
// Confirm dialogs are handled by a Svelte ConfirmDialog component (shared with web UI).
const desktopJS = `
document.addEventListener('click', function(e) {
  const a = e.target.closest('a[target="_blank"]');
  if (!a) return;
  e.preventDefault();
  fetch('/open-url', {method:'POST', headers:{'Content-Type':'application/x-www-form-urlencoded'}, body:'url='+encodeURIComponent(a.href)});
});
`
