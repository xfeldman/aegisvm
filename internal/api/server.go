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
	"github.com/xfeldman/aegis/internal/logstore"
	"github.com/xfeldman/aegis/internal/overlay"
	"github.com/xfeldman/aegis/internal/registry"
	"github.com/xfeldman/aegis/internal/secrets"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Server is the aegisd HTTP API server.
type Server struct {
	cfg         *config.Config
	vmm         vmm.VMM
	tasks       *TaskStore
	lifecycle   *lifecycle.Manager
	registry    *registry.DB
	imageCache  *image.Cache
	overlay     overlay.Overlay
	secretStore *secrets.Store
	logStore    *logstore.Store
	mux         *http.ServeMux
	server      *http.Server
	ln          net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM, lm *lifecycle.Manager, reg *registry.DB, imgCache *image.Cache, ov overlay.Overlay, ss *secrets.Store, ls *logstore.Store) *Server {
	s := &Server{
		cfg:         cfg,
		vmm:         v,
		tasks:       NewTaskStore(v, cfg, imgCache, ov),
		lifecycle:   lm,
		registry:    reg,
		imageCache:  imgCache,
		overlay:     ov,
		secretStore: ss,
		logStore:    ls,
		mux:         http.NewServeMux(),
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
	s.mux.HandleFunc("GET /v1/instances", s.handleListInstances)
	s.mux.HandleFunc("GET /v1/instances/{id}", s.handleGetInstance)
	s.mux.HandleFunc("GET /v1/instances/{id}/logs", s.handleInstanceLogs)
	s.mux.HandleFunc("POST /v1/instances/{id}/exec", s.handleExecInstance)
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

	// Secret routes (M3)
	s.mux.HandleFunc("PUT /v1/apps/{id}/secrets/{name}", s.handleSetSecret)
	s.mux.HandleFunc("GET /v1/apps/{id}/secrets", s.handleListSecrets)
	s.mux.HandleFunc("DELETE /v1/apps/{id}/secrets/{name}", s.handleDeleteSecret)
	s.mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSetWorkspaceSecret)
	s.mux.HandleFunc("GET /v1/secrets", s.handleListWorkspaceSecrets)

	// Kit routes (M3)
	s.mux.HandleFunc("POST /v1/kits", s.handleRegisterKit)
	s.mux.HandleFunc("GET /v1/kits", s.handleListKits)
	s.mux.HandleFunc("GET /v1/kits/{name}", s.handleGetKit)
	s.mux.HandleFunc("DELETE /v1/kits/{name}", s.handleDeleteKit)
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
	Status       string                 `json:"status"`
	Backend      string                 `json:"backend"`
	Capabilities map[string]interface{} `json:"capabilities"`
	KitCount     int                    `json:"kit_count"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	caps := s.vmm.Capabilities()

	kitCount := 0
	if kits, err := s.registry.ListKits(); err == nil {
		kitCount = len(kits)
	}

	writeJSON(w, http.StatusOK, statusResponse{
		Status:  "running",
		Backend: caps.Name,
		Capabilities: map[string]interface{}{
			"pause_resume":          caps.Pause,
			"memory_snapshots":      caps.SnapshotRestore,
			"boot_from_disk_layers": true,
		},
		KitCount: kitCount,
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

	resp := map[string]interface{}{
		"id":                inst.ID,
		"state":             inst.State,
		"command":           inst.Command,
		"app_id":            inst.AppID,
		"release_id":        inst.ReleaseID,
		"created_at":        inst.CreatedAt.Format(time.RFC3339),
		"last_active_at":    s.lifecycle.LastActivity(inst.ID).Format(time.RFC3339),
		"active_connections": s.lifecycle.ActiveConns(inst.ID),
	}

	if len(inst.ExposePorts) > 0 {
		ports := make([]int, len(inst.ExposePorts))
		for i, p := range inst.ExposePorts {
			ports[i] = p.GuestPort
		}
		resp["expose_ports"] = ports
	}

	if len(inst.Endpoints) > 0 {
		eps := make([]map[string]interface{}, len(inst.Endpoints))
		for i, ep := range inst.Endpoints {
			eps[i] = map[string]interface{}{
				"guest_port": ep.GuestPort,
				"host_port":  ep.HostPort,
				"protocol":   ep.Protocol,
			}
		}
		resp["endpoints"] = eps
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances := s.lifecycle.ListInstances()

	result := make([]map[string]interface{}, 0, len(instances))
	for _, inst := range instances {
		result = append(result, map[string]interface{}{
			"id":                inst.ID,
			"state":             inst.State,
			"app_id":            inst.AppID,
			"command":           inst.Command,
			"created_at":        inst.CreatedAt.Format(time.RFC3339),
			"last_active_at":    s.lifecycle.LastActivity(inst.ID).Format(time.RFC3339),
			"active_connections": s.lifecycle.ActiveConns(inst.ID),
		})
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleInstanceLogs(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	sinceStr := r.URL.Query().Get("since")
	tailStr := r.URL.Query().Get("tail")
	execIDFilter := r.URL.Query().Get("exec_id")

	var since time.Time
	if sinceStr != "" {
		since, _ = time.Parse(time.RFC3339, sinceStr)
	}
	tail := 0
	if tailStr != "" {
		fmt.Sscanf(tailStr, "%d", &tail)
	}

	il := s.logStore.Get(id)
	if il == nil {
		// No logs yet — return empty
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if follow {
		ch, existing, unsub := il.Subscribe()
		defer unsub()

		// Stream existing entries
		for _, e := range existing {
			if execIDFilter != "" && e.ExecID != execIDFilter {
				continue
			}
			streamJSON(w, e)
		}

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-ch:
				if !ok {
					return
				}
				if execIDFilter != "" && entry.ExecID != execIDFilter {
					continue
				}
				streamJSON(w, entry)
			}
		}
	} else {
		entries := il.Read(since, tail)
		for _, e := range entries {
			if execIDFilter != "" && e.ExecID != execIDFilter {
				continue
			}
			streamJSON(w, e)
		}
	}
}

func (s *Server) handleExecInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	var req struct {
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}

	execID, startedAt, doneCh, err := s.lifecycle.ExecInstance(r.Context(), id, req.Command, req.Env)
	if err != nil {
		if err == lifecycle.ErrInstanceStopped {
			writeError(w, http.StatusConflict, "instance is stopped")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Stream the exec output inline
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Write the exec info as first line
	streamJSON(w, map[string]interface{}{
		"exec_id":    execID,
		"started_at": startedAt.Format(time.RFC3339),
	})

	// Subscribe to logs filtered by exec_id
	il := s.logStore.Get(id)
	if il == nil {
		// Wait for done even without logs
		select {
		case exitCode := <-doneCh:
			streamJSON(w, map[string]interface{}{"exec_id": execID, "exit_code": exitCode, "done": true})
		case <-r.Context().Done():
		}
		return
	}

	logCh, existing, unsub := il.Subscribe()
	defer unsub()

	// Stream any existing logs for this exec
	for _, e := range existing {
		if e.ExecID == execID {
			streamJSON(w, e)
		}
	}

	// Stream live logs until exec completes or client disconnects
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case exitCode := <-doneCh:
			// Exec finished — drain any remaining buffered logs then send done
			for {
				select {
				case entry := <-logCh:
					if entry.ExecID == execID {
						streamJSON(w, entry)
					}
				default:
					streamJSON(w, map[string]interface{}{
						"exec_id":   execID,
						"exit_code": exitCode,
						"done":      true,
					})
					return
				}
			}
		case entry, ok := <-logCh:
			if !ok {
				return
			}
			if entry.ExecID == execID {
				streamJSON(w, entry)
			}
		}
	}
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
