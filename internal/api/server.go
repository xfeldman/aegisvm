package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/image"
	"github.com/xfeldman/aegis/internal/lifecycle"
	"github.com/xfeldman/aegis/internal/overlay"
	"github.com/xfeldman/aegis/internal/registry"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Server is the aegisd HTTP API server.
type Server struct {
	cfg        *config.Config
	vmm        vmm.VMM
	tasks      *TaskStore
	lifecycle  *lifecycle.Manager
	registry   *registry.DB
	imageCache *image.Cache
	overlay    overlay.Overlay
	mux        *http.ServeMux
	server     *http.Server
	ln         net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM, lm *lifecycle.Manager, reg *registry.DB, imgCache *image.Cache, ov overlay.Overlay) *Server {
	s := &Server{
		cfg:        cfg,
		vmm:        v,
		tasks:      NewTaskStore(v, cfg, imgCache, ov),
		lifecycle:  lm,
		registry:   reg,
		imageCache: imgCache,
		overlay:    ov,
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	s.server = &http.Server{Handler: s.mux}
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /v1/tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("GET /v1/tasks/{id}/logs", s.handleGetTaskLogs)
	s.mux.HandleFunc("POST /v1/instances", s.handleCreateInstance)
	s.mux.HandleFunc("GET /v1/instances/{id}", s.handleGetInstance)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)

	// App routes (M2)
	s.mux.HandleFunc("POST /v1/apps", s.handleCreateApp)
	s.mux.HandleFunc("GET /v1/apps", s.handleListApps)
	s.mux.HandleFunc("GET /v1/apps/{id}", s.handleGetApp)
	s.mux.HandleFunc("DELETE /v1/apps/{id}", s.handleDeleteApp)
	s.mux.HandleFunc("POST /v1/apps/{id}/publish", s.handlePublishApp)
	s.mux.HandleFunc("GET /v1/apps/{id}/releases", s.handleListReleases)
	s.mux.HandleFunc("POST /v1/apps/{id}/serve", s.handleServeApp)
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

// Instance API types

type createInstanceRequest struct {
	Command     []string `json:"command"`
	ExposePorts []int    `json:"expose_ports"`
}

type instanceResponse struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	Command     []string `json:"command"`
	ExposePorts []int    `json:"expose_ports"`
	RouterAddr  string   `json:"router_addr,omitempty"`
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req createInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if len(req.ExposePorts) == 0 {
		writeError(w, http.StatusBadRequest, "expose_ports is required")
		return
	}

	id := fmt.Sprintf("inst-%d", time.Now().UnixNano())

	// Build PortExpose list
	var exposePorts []vmm.PortExpose
	for _, p := range req.ExposePorts {
		exposePorts = append(exposePorts, vmm.PortExpose{
			GuestPort: p,
			Protocol:  "http",
		})
	}

	// Create in lifecycle manager
	s.lifecycle.CreateInstance(id, req.Command, exposePorts)

	// Persist to registry
	if s.registry != nil {
		regInst := &registry.Instance{
			ID:          id,
			State:       "stopped",
			Command:     req.Command,
			ExposePorts: req.ExposePorts,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := s.registry.SaveInstance(regInst); err != nil {
			log.Printf("save instance to registry: %v", err)
		}
	}

	// Boot the instance
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.lifecycle.EnsureInstance(ctx, id); err != nil {
			log.Printf("instance %s boot failed: %v", id, err)
		}
	}()

	writeJSON(w, http.StatusCreated, instanceResponse{
		ID:          id,
		State:       "starting",
		Command:     req.Command,
		ExposePorts: req.ExposePorts,
		RouterAddr:  s.cfg.RouterAddr,
	})
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":    inst.ID,
		"state": inst.State,
	})
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.StopInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.DeleteInstance(id)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
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
