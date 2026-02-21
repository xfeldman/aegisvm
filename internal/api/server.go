package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/lifecycle"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/router"
	"github.com/xfeldman/aegisvm/internal/secrets"
	"github.com/xfeldman/aegisvm/internal/vmm"
)

// Server is the aegisd HTTP API server.
type Server struct {
	cfg         *config.Config
	vmm         vmm.VMM
	lifecycle   *lifecycle.Manager
	registry    *registry.DB
	secretStore *secrets.Store
	logStore    *logstore.Store
	router      *router.Router
	mux         *http.ServeMux
	server      *http.Server
	ln          net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM, lm *lifecycle.Manager, reg *registry.DB, ss *secrets.Store, ls *logstore.Store, rtr *router.Router) *Server {
	s := &Server{
		cfg:         cfg,
		vmm:         v,
		lifecycle:   lm,
		registry:    reg,
		secretStore: ss,
		logStore:    ls,
		router:      rtr,
		mux:         http.NewServeMux(),
	}
	s.registerRoutes()
	s.server = &http.Server{Handler: s.mux}
	return s
}

func (s *Server) registerRoutes() {
	// Instance routes
	s.mux.HandleFunc("POST /v1/instances", s.handleCreateInstance)
	s.mux.HandleFunc("GET /v1/instances", s.handleListInstances)
	s.mux.HandleFunc("GET /v1/instances/{id}", s.handleGetInstance)
	s.mux.HandleFunc("GET /v1/instances/{id}/logs", s.handleInstanceLogs)
	s.mux.HandleFunc("POST /v1/instances/{id}/exec", s.handleExecInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/start", s.handleStartInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/disable", s.handleDisableInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/pause", s.handlePauseInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/resume", s.handleResumeInstance)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)
	s.mux.HandleFunc("POST /v1/instances/prune", s.handlePruneInstances)

	// Secret routes (workspace-scoped key-value store)
	s.mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSetSecret)
	s.mux.HandleFunc("GET /v1/secrets", s.handleListSecrets)
	s.mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleDeleteSecret)

	// Status
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
	Status       string                 `json:"status"`
	Backend      string                 `json:"backend"`
	Capabilities map[string]interface{} `json:"capabilities"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	caps := s.vmm.Capabilities()

	writeJSON(w, http.StatusOK, statusResponse{
		Status:  "running",
		Backend: caps.Name,
		Capabilities: map[string]interface{}{
			"pause_resume":          caps.Pause,
			"boot_from_disk_layers": true,
			"network_backend":       caps.NetworkBackend,
		},
	})
}

// Instance API types

type exposeRequest struct {
	Port       int    `json:"port"`
	PublicPort int    `json:"public_port,omitempty"` // 0 = random
	Protocol   string `json:"protocol,omitempty"`
}

type createInstanceRequest struct {
	ImageRef  string            `json:"image_ref,omitempty"`
	Command   []string          `json:"command"`
	Exposes   []exposeRequest   `json:"exposes,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   []string          `json:"secrets,omitempty"` // [] = none, ["*"] = all, ["KEY1","KEY2"] = allowlist
	Handle    string            `json:"handle,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	MemoryMB  int               `json:"memory_mb,omitempty"`
	VCPUs     int               `json:"vcpus,omitempty"`
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req createInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	// Idempotent start: if handle exists, restart if stopped or conflict if running
	if req.Handle != "" {
		if existing := s.lifecycle.GetInstanceByHandle(req.Handle); existing != nil {
			s.handleRestartOrConflict(w, existing, req)
			return
		}
	}

	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}

	id := fmt.Sprintf("inst-%d", time.Now().UnixNano())

	// Build PortExpose list
	var exposePorts []vmm.PortExpose
	for _, e := range req.Exposes {
		proto := e.Protocol
		if proto == "" {
			proto = "http"
		}
		exposePorts = append(exposePorts, vmm.PortExpose{
			GuestPort: e.Port,
			Protocol:  proto,
		})
	}

	// Build options
	var opts []lifecycle.InstanceOption
	if req.Handle != "" {
		opts = append(opts, lifecycle.WithHandle(req.Handle))
	}
	if req.ImageRef != "" {
		opts = append(opts, lifecycle.WithImageRef(req.ImageRef))
	}
	// Resolve secrets + env into final env map
	env := s.resolveEnv(req.Secrets, req.Env)
	if len(env) > 0 {
		opts = append(opts, lifecycle.WithEnv(env))
	}
	if req.Workspace != "" {
		resolved := s.resolveWorkspace(req.Workspace)
		os.MkdirAll(resolved, 0755)
		req.Workspace = resolved
		opts = append(opts, lifecycle.WithWorkspace(resolved))
	}
	if req.MemoryMB > 0 {
		opts = append(opts, lifecycle.WithMemory(req.MemoryMB))
	}
	if req.VCPUs > 0 {
		opts = append(opts, lifecycle.WithVCPUs(req.VCPUs))
	}

	// Create in lifecycle manager
	s.lifecycle.CreateInstance(id, req.Command, exposePorts, opts...)

	// Allocate public ports via router
	var publicEndpoints []router.PublicEndpoint
	if s.router != nil && len(req.Exposes) > 0 {
		for i, e := range req.Exposes {
			proto := exposePorts[i].Protocol
			publicPort, err := s.router.AllocatePort(id, e.Port, e.PublicPort, proto)
			if err != nil {
				log.Printf("allocate public port for guest %d: %v", e.Port, err)
				s.router.FreeAllPorts(id)
				s.lifecycle.DeleteInstance(id)
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("port allocation failed: %v", err))
				return
			}
			publicEndpoints = append(publicEndpoints, router.PublicEndpoint{
				GuestPort:  e.Port,
				PublicPort: publicPort,
				Protocol:   proto,
			})
		}
	}

	// Persist to registry (after port allocation so we can save public ports)
	if s.registry != nil {
		portInts := make([]int, len(exposePorts))
		for i, p := range exposePorts {
			portInts[i] = p.GuestPort
		}
		publicPorts := make(map[int]int)
		for _, ep := range publicEndpoints {
			publicPorts[ep.GuestPort] = ep.PublicPort
		}
		regInst := &registry.Instance{
			ID:          id,
			State:       "stopped",
			Command:     req.Command,
			ExposePorts: portInts,
			Handle:      req.Handle,
			ImageRef:    req.ImageRef,
			Workspace:   req.Workspace,
			Env:         env,
			SecretKeys:  req.Secrets,
			PublicPorts: publicPorts,
			Enabled:     true,
			MemoryMB:    req.MemoryMB,
			VCPUs:       req.VCPUs,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := s.registry.SaveInstance(regInst); err != nil {
			log.Printf("save instance to registry: %v", err)
		}
	}

	// Boot the instance
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.lifecycle.EnsureInstance(ctx, id); err != nil {
			log.Printf("instance %s boot failed: %v", id, err)
		}
	}()

	resp := map[string]interface{}{
		"id":      id,
		"state":   "starting",
		"command": req.Command,
	}
	if req.Handle != "" {
		resp["handle"] = req.Handle
	}
	if req.ImageRef != "" {
		resp["image_ref"] = req.ImageRef
	}
	if len(publicEndpoints) > 0 {
		eps := make([]map[string]interface{}, len(publicEndpoints))
		for i, ep := range publicEndpoints {
			eps[i] = map[string]interface{}{
				"guest_port":  ep.GuestPort,
				"public_port": ep.PublicPort,
				"protocol":    ep.Protocol,
			}
		}
		resp["endpoints"] = eps
	}
	if len(exposePorts) > 0 {
		ports := make([]int, len(exposePorts))
		for i, p := range exposePorts {
			ports[i] = p.GuestPort
		}
		resp["expose_ports"] = ports
	}

	writeJSON(w, http.StatusCreated, resp)
}

// handleRestartOrConflict handles idempotent instance start:
// STOPPED → restart using stored config
// RUNNING/STARTING/PAUSED → 409 conflict
func (s *Server) handleRestartOrConflict(w http.ResponseWriter, inst *lifecycle.Instance, req createInstanceRequest) {
	if inst.State != lifecycle.StateStopped {
		writeError(w, http.StatusConflict, fmt.Sprintf("instance %q is %s", req.Handle, inst.State))
		return
	}

	s.ensurePortListeners(inst)

	// Re-enable and boot via StartInstance (sets Enabled=true + boots).
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.lifecycle.StartInstance(ctx, inst.ID); err != nil {
			log.Printf("instance %s restart failed: %v", inst.ID, err)
		}
	}()

	if s.registry != nil {
		s.registry.UpdateEnabled(inst.ID, true)
	}

	resp := map[string]interface{}{
		"id":      inst.ID,
		"state":   "starting",
		"command": inst.Command,
	}
	if inst.HandleAlias != "" {
		resp["handle"] = inst.HandleAlias
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	// Try by ID first, then by handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	resp := map[string]interface{}{
		"id":                inst.ID,
		"state":             inst.State,
		"enabled":           inst.Enabled,
		"command":           inst.Command,
		"created_at":        inst.CreatedAt.Format(time.RFC3339),
		"last_active_at":    s.lifecycle.LastActivity(inst.ID).Format(time.RFC3339),
		"active_connections": s.lifecycle.ActiveConns(inst.ID),
	}

	if inst.HandleAlias != "" {
		resp["handle"] = inst.HandleAlias
	}
	if inst.ImageRef != "" {
		resp["image_ref"] = inst.ImageRef
	}
	if !inst.StoppedAt.IsZero() {
		resp["stopped_at"] = inst.StoppedAt.Format(time.RFC3339)
	}

	if len(inst.ExposePorts) > 0 {
		ports := make([]int, len(inst.ExposePorts))
		for i, p := range inst.ExposePorts {
			ports[i] = p.GuestPort
		}
		resp["expose_ports"] = ports
	}
	// Show public (router-owned) endpoints
	if s.router != nil {
		publicEps := s.router.GetAllPublicPorts(inst.ID)
		if len(publicEps) > 0 {
			eps := make([]map[string]interface{}, len(publicEps))
			for i, ep := range publicEps {
				eps[i] = map[string]interface{}{
					"guest_port":  ep.GuestPort,
					"public_port": ep.PublicPort,
					"protocol":    ep.Protocol,
				}
			}
			resp["endpoints"] = eps
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances := s.lifecycle.ListInstances()

	// Optional state filter: ?state=stopped or ?state=running
	stateFilter := r.URL.Query().Get("state")

	result := make([]map[string]interface{}, 0, len(instances))
	for _, inst := range instances {
		if stateFilter != "" && inst.State != stateFilter {
			continue
		}
		entry := map[string]interface{}{
			"id":                inst.ID,
			"state":             inst.State,
			"enabled":           inst.Enabled,
			"command":           inst.Command,
			"created_at":        inst.CreatedAt.Format(time.RFC3339),
			"last_active_at":    s.lifecycle.LastActivity(inst.ID).Format(time.RFC3339),
			"active_connections": s.lifecycle.ActiveConns(inst.ID),
		}
		if inst.HandleAlias != "" {
			entry["handle"] = inst.HandleAlias
		}
		if inst.ImageRef != "" {
			entry["image_ref"] = inst.ImageRef
		}
		if !inst.StoppedAt.IsZero() {
			entry["stopped_at"] = inst.StoppedAt.Format(time.RFC3339)
		}
		result = append(result, entry)
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleInstanceLogs(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if !inst.Enabled {
		writeError(w, http.StatusConflict, "instance is disabled")
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

	il := s.logStore.Get(inst.ID)
	if il == nil {
		// No logs yet — return empty
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

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

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

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

	execID, startedAt, doneCh, err := s.lifecycle.ExecInstance(r.Context(), inst.ID, req.Command, req.Env)
	if err != nil {
		if err == lifecycle.ErrInstanceDisabled {
			writeError(w, http.StatusConflict, "instance is disabled")
			return
		}
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
	il := s.logStore.Get(inst.ID)
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

func (s *Server) handlePauseInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.PauseInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleResumeInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.ResumeInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

// ensurePortListeners re-allocates port listeners for an instance if they
// were freed (e.g. by disable). Uses saved public ports from registry for
// port stability. No-op if listeners already exist.
func (s *Server) ensurePortListeners(inst *lifecycle.Instance) {
	if s.router == nil {
		return
	}
	existing := s.router.GetAllPublicPorts(inst.ID)
	if len(existing) > 0 || len(inst.ExposePorts) == 0 {
		return
	}
	var savedPorts map[int]int
	if s.registry != nil {
		if ri, err := s.registry.GetInstance(inst.ID); err == nil && ri != nil {
			savedPorts = ri.PublicPorts
		}
	}
	for _, ep := range inst.ExposePorts {
		requestedPort := 0
		if savedPorts != nil {
			requestedPort = savedPorts[ep.GuestPort]
		}
		if _, err := s.router.AllocatePort(inst.ID, ep.GuestPort, requestedPort, ep.Protocol); err != nil {
			if requestedPort > 0 {
				s.router.AllocatePort(inst.ID, ep.GuestPort, 0, ep.Protocol)
			}
		}
	}
}

func (s *Server) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	s.ensurePortListeners(inst)

	// StartInstance sets Enabled=true and boots
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.lifecycle.StartInstance(ctx, inst.ID); err != nil {
			log.Printf("instance %s start failed: %v", inst.ID, err)
		}
	}()

	if s.registry != nil {
		s.registry.UpdateEnabled(inst.ID, true)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "starting"})
}

func (s *Server) handleDisableInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	// Free port listeners immediately (no new connections)
	if s.router != nil {
		s.router.FreeAllPorts(inst.ID)
	}

	if err := s.lifecycle.DisableInstance(inst.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.UpdateEnabled(inst.ID, false)
		s.registry.UpdateState(inst.ID, "stopped")
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")

	// Free public port listeners before deleting instance
	if s.router != nil {
		s.router.FreeAllPorts(id)
	}

	if err := s.lifecycle.DeleteInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.DeleteInstance(id)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePruneInstances(w http.ResponseWriter, r *http.Request) {
	olderThanStr := r.URL.Query().Get("older_than")
	if olderThanStr == "" {
		writeError(w, http.StatusBadRequest, "older_than query parameter is required (e.g. 7d, 24h)")
		return
	}

	dur, err := parseDuration(olderThanStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid duration: %s", olderThanStr))
		return
	}

	cutoff := time.Now().Add(-dur)
	instances := s.lifecycle.ListInstances()
	var pruned []string

	for _, inst := range instances {
		if inst.State == lifecycle.StateStopped && !inst.StoppedAt.IsZero() && inst.StoppedAt.Before(cutoff) {
			if s.router != nil {
				s.router.FreeAllPorts(inst.ID)
			}
			if err := s.lifecycle.DeleteInstance(inst.ID); err == nil {
				if s.registry != nil {
					s.registry.DeleteInstance(inst.ID)
				}
				pruned = append(pruned, inst.ID)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pruned": len(pruned),
		"ids":    pruned,
	})
}

// parseDuration extends time.ParseDuration to support "d" for days.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		n, err := fmt.Sscanf(days, "%d", new(int))
		if err != nil || n != 1 {
			return 0, fmt.Errorf("invalid day duration: %s", s)
		}
		var d int
		fmt.Sscanf(days, "%d", &d)
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// Secret handlers — dumb key-value store with encryption.
// No scoping, no naming rules, no rotation.

type setSecretRequest struct {
	Value string `json:"value"`
}

type secretResponse struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")

	var req setSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}

	encrypted, err := s.secretStore.EncryptString(req.Value)
	if err != nil {
		log.Printf("encrypt secret: %v", err)
		writeError(w, http.StatusInternalServerError, "encryption failed")
		return
	}

	secret := &registry.Secret{
		ID:             fmt.Sprintf("sec-%d", time.Now().UnixNano()),
		Name:           name,
		EncryptedValue: encrypted,
		CreatedAt:      time.Now(),
	}

	if err := s.registry.SaveSecret(secret); err != nil {
		log.Printf("save secret: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save secret")
		return
	}

	log.Printf("secret set: %s", name)
	writeJSON(w, http.StatusOK, secretResponse{
		Name:      secret.Name,
		CreatedAt: secret.CreatedAt.Format(time.RFC3339),
	})
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	secs, err := s.registry.ListSecrets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list secrets: %v", err))
		return
	}

	resp := make([]secretResponse, 0, len(secs))
	for _, sec := range secs {
		resp = append(resp, secretResponse{
			Name:      sec.Name,
			CreatedAt: sec.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	if err := s.registry.DeleteSecretByName(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("secret deleted: %s", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// resolveEnv builds the final env map for an instance by resolving secrets
// from the allowlist and merging explicit env vars on top.
//
//   - secretKeys == nil or []  → inject no secrets
//   - secretKeys == ["*"]      → inject all secrets
//   - secretKeys == ["A","B"]  → inject only named secrets
//
// Explicit env vars always override secrets on name collision.
func (s *Server) resolveEnv(secretKeys []string, explicitEnv map[string]string) map[string]string {
	env := make(map[string]string)

	if s.secretStore != nil && len(secretKeys) > 0 {
		injectAll := len(secretKeys) == 1 && secretKeys[0] == "*"
		var allowlist map[string]bool
		if !injectAll {
			allowlist = make(map[string]bool, len(secretKeys))
			for _, k := range secretKeys {
				allowlist[k] = true
			}
		}
		secrets, _ := s.registry.ListSecrets()
		for _, sec := range secrets {
			if injectAll || allowlist[sec.Name] {
				val, err := s.secretStore.DecryptString(sec.EncryptedValue)
				if err == nil {
					env[sec.Name] = val
				}
			}
		}
	}

	// Explicit env overrides secrets
	for k, v := range explicitEnv {
		env[k] = v
	}
	return env
}

// resolveWorkspace resolves a workspace argument to an absolute path.
// Named workspaces (no / or . prefix) resolve to ~/.aegis/data/workspaces/<name>.
// Path workspaces resolve to their absolute path.
func (s *Server) resolveWorkspace(ws string) string {
	if !strings.Contains(ws, "/") && !strings.HasPrefix(ws, ".") {
		return filepath.Join(s.cfg.WorkspacesDir, ws)
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return ws
	}
	return abs
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
