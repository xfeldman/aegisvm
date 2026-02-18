package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Server is the aegisd HTTP API server.
type Server struct {
	cfg    *config.Config
	vmm    vmm.VMM
	tasks  *TaskStore
	mux    *http.ServeMux
	server *http.Server
	ln     net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM) *Server {
	s := &Server{
		cfg:   cfg,
		vmm:   v,
		tasks: NewTaskStore(v, cfg),
		mux:   http.NewServeMux(),
	}
	s.registerRoutes()
	s.server = &http.Server{Handler: s.mux}
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /v1/tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("GET /v1/tasks/{id}/logs", s.handleGetTaskLogs)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
}

// Start begins listening on the unix socket.
func (s *Server) Start() error {
	// Remove stale socket
	os.Remove(s.cfg.SocketPath)

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	s.ln = ln

	// Make socket accessible
	os.Chmod(s.cfg.SocketPath, 0600)

	log.Printf("aegisd API listening on %s", s.cfg.SocketPath)

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Status response

type statusResponse struct {
	Status   string `json:"status"`
	Backend  string `json:"backend"`
	Platform string `json:"platform"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	caps := s.vmm.Capabilities()
	writeJSON(w, http.StatusOK, statusResponse{
		Status:  "running",
		Backend: caps.Name,
	})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// pathParam extracts a path parameter from the request.
// For Go 1.22+ with "GET /v1/tasks/{id}" patterns.
func pathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}

// streamJSON writes newline-delimited JSON values to a flushing writer.
func streamJSON(w http.ResponseWriter, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

// isValidID checks if an ID string is safe.
func isValidID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return !strings.Contains(id, "..")
}
