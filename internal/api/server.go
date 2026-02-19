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
	"github.com/xfeldman/aegis/internal/lifecycle"
	"github.com/xfeldman/aegis/internal/logstore"
	"github.com/xfeldman/aegis/internal/registry"
	"github.com/xfeldman/aegis/internal/secrets"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Server is the aegisd HTTP API server.
type Server struct {
	cfg         *config.Config
	vmm         vmm.VMM
	lifecycle   *lifecycle.Manager
	registry    *registry.DB
	secretStore *secrets.Store
	logStore    *logstore.Store
	mux         *http.ServeMux
	server      *http.Server
	ln          net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM, lm *lifecycle.Manager, reg *registry.DB, ss *secrets.Store, ls *logstore.Store) *Server {
	s := &Server{
		cfg:         cfg,
		vmm:         v,
		lifecycle:   lm,
		registry:    reg,
		secretStore: ss,
		logStore:    ls,
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
	s.mux.HandleFunc("POST /v1/instances/{id}/stop", s.handleStopInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/pause", s.handlePauseInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/resume", s.handleResumeInstance)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)

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
			"memory_snapshots":      caps.SnapshotRestore,
			"boot_from_disk_layers": true,
		},
	})
}

// Instance API types

type exposeRequest struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

type createInstanceRequest struct {
	ImageRef  string            `json:"image_ref,omitempty"`
	Command   []string          `json:"command"`
	Exposes   []exposeRequest   `json:"exposes,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   []string          `json:"secrets,omitempty"` // [] = none, ["*"] = all, ["KEY1","KEY2"] = allowlist
	Handle    string            `json:"handle,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
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
		opts = append(opts, lifecycle.WithWorkspace(req.Workspace))
	}

	// Create in lifecycle manager
	s.lifecycle.CreateInstance(id, req.Command, exposePorts, opts...)

	// Persist to registry
	if s.registry != nil {
		portInts := make([]int, len(exposePorts))
		for i, p := range exposePorts {
			portInts[i] = p.GuestPort
		}
		regInst := &registry.Instance{
			ID:          id,
			State:       "stopped",
			Command:     req.Command,
			ExposePorts: portInts,
			Handle:      req.Handle,
			ImageRef:    req.ImageRef,
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
		"id":         id,
		"state":      "starting",
		"command":    req.Command,
		"router_addr": s.cfg.RouterAddr,
	}
	if req.Handle != "" {
		resp["handle"] = req.Handle
	}
	if req.ImageRef != "" {
		resp["image_ref"] = req.ImageRef
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
		entry := map[string]interface{}{
			"id":                inst.ID,
			"state":             inst.State,
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

func (s *Server) handleStopInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.StopInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.UpdateState(id, "stopped")
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.DeleteInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.DeleteInstance(id)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Secret handlers — dumb key-value store with encryption.
// No scoping, no naming rules, no rotation, no kit semantics.

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
