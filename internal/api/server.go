package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/kit"
	"github.com/xfeldman/aegisvm/internal/daemon"
	"github.com/xfeldman/aegisvm/internal/lifecycle"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/router"
	"github.com/xfeldman/aegisvm/internal/secrets"
	"github.com/xfeldman/aegisvm/internal/tether"
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
	daemons     *daemon.Manager
	mux         *http.ServeMux
	server      *http.Server
	ln          net.Listener
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, v vmm.VMM, lm *lifecycle.Manager, reg *registry.DB, ss *secrets.Store, ls *logstore.Store, rtr *router.Router, dm *daemon.Manager) *Server {
	s := &Server{
		cfg:         cfg,
		vmm:         v,
		lifecycle:   lm,
		registry:    reg,
		secretStore: ss,
		logStore:    ls,
		router:      rtr,
		daemons:     dm,
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
	s.mux.HandleFunc("POST /v1/instances/{id}/expose", s.handleExposePort)
	s.mux.HandleFunc("DELETE /v1/instances/{id}/expose/{guest_port}", s.handleUnexposePort)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)
	s.mux.HandleFunc("POST /v1/instances/prune", s.handlePruneInstances)

	// Workspace file access
	s.mux.HandleFunc("GET /v1/instances/{id}/workspace", s.handleWorkspaceRead)
	s.mux.HandleFunc("POST /v1/instances/{id}/workspace", s.handleWorkspaceWrite)

	// Kit config file access (host-side, ~/.aegis/kits/{handle}/)
	s.mux.HandleFunc("GET /v1/instances/{id}/kit-config", s.handleKitConfigRead)
	s.mux.HandleFunc("POST /v1/instances/{id}/kit-config", s.handleKitConfigWrite)

	// Tether routes (Agent Kit messaging)
	s.mux.HandleFunc("POST /v1/instances/{id}/tether", s.handleTetherIngress)
	s.mux.HandleFunc("GET /v1/instances/{id}/tether/stream", s.handleTetherStream)
	s.mux.HandleFunc("GET /v1/instances/{id}/tether/poll", s.handleTetherPoll)

	// Secret routes (workspace-scoped key-value store)
	s.mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSetSecret)
	s.mux.HandleFunc("GET /v1/secrets", s.handleListSecrets)
	s.mux.HandleFunc("GET /v1/secrets/{name}", s.handleGetSecret)
	s.mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleDeleteSecret)

	// Kits
	s.mux.HandleFunc("GET /v1/kits", s.handleListKits)

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

	// Socket permissions: default (0755 from net.Listen) is fine.
	// When running via sudo, chownToInvokingUser() in aegisd makes the
	// socket user-owned, so the CLI can connect without permission hacks.

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
			"persistent_pause":      caps.PersistentPause,
			"boot_from_disk_layers": true,
			"network_backend":       caps.NetworkBackend,
		},
	})
}

func (s *Server) handleListKits(w http.ResponseWriter, r *http.Request) {
	manifests, err := kit.ListManifests()
	if err != nil {
		manifests = nil
	}
	result := make([]map[string]interface{}, 0, len(manifests))
	for _, m := range manifests {
		entry := map[string]interface{}{
			"name": m.Name,
		}
		if m.Version != "" {
			entry["version"] = m.Version
		}
		if m.Description != "" {
			entry["description"] = m.Description
		}
		if m.Image.Base != "" {
			entry["image"] = m.Image.Base
		}
		if len(m.Defaults.Command) > 0 {
			entry["defaults"] = map[string]interface{}{
				"command": m.Defaults.Command,
			}
		}
		// Flatten daemon configs (host) first, then kit-level configs (workspace).
		var cfgs []map[string]interface{}
		for _, d := range m.InstanceDaemons {
			if d.Config != nil {
				cfg := map[string]interface{}{
					"path":     d.Config.Path,
					"location": "host",
				}
				if d.Config.Label != "" {
					cfg["label"] = d.Config.Label
				}
				if d.Config.Default != nil {
					cfg["default"] = json.RawMessage(d.Config.Default)
				}
				cfgs = append(cfgs, cfg)
			}
		}
		for _, c := range m.Config {
			cfg := map[string]interface{}{
				"path":     c.Path,
				"location": "workspace",
			}
			if c.Label != "" {
				cfg["label"] = c.Label
			}
			if c.Default != nil {
				cfg["default"] = json.RawMessage(c.Default)
			}
			cfgs = append(cfgs, cfg)
		}
		if len(cfgs) > 0 {
			entry["config"] = cfgs
		}
		// Extract env vars referenced by configs (any *_env field value)
		if envRefs := extractReferencedEnv(m); len(envRefs) > 0 {
			entry["referenced_env"] = envRefs
		}
		result = append(result, entry)
	}
	writeJSON(w, http.StatusOK, result)
}

// extractReferencedEnv scans all config defaults in a kit manifest for fields
// ending in "_env" and returns the unique string values. This tells callers
// (MCP, UI) which env vars the kit's configs expect.
func extractReferencedEnv(m *kit.Manifest) []string {
	seen := make(map[string]bool)
	var result []string

	collect := func(raw json.RawMessage) {
		if raw == nil {
			return
		}
		scanEnvFields(raw, seen, &result)
	}

	for _, c := range m.Config {
		collect(c.Default)
	}
	for _, d := range m.InstanceDaemons {
		if d.Config != nil {
			collect(d.Config.Default)
		}
	}
	return result
}

// scanEnvFields recursively scans a JSON value for keys ending in "_env"
// and collects their string values.
func scanEnvFields(raw json.RawMessage, seen map[string]bool, result *[]string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	for key, val := range obj {
		if strings.HasSuffix(key, "_env") {
			var s string
			if json.Unmarshal(val, &s) == nil && s != "" && !seen[s] {
				seen[s] = true
				*result = append(*result, s)
			}
		} else {
			// Recurse into nested objects
			scanEnvFields(val, seen, result)
		}
	}
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
	Env       map[string]string `json:"env,omitempty"`
	Secrets   []string          `json:"secrets,omitempty"` // [] = none, ["*"] = all, ["KEY1","KEY2"] = allowlist
	Handle    string            `json:"handle,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
	VCPUs      int               `json:"vcpus,omitempty"`
	IdlePolicy   string                   `json:"idle_policy,omitempty"`
	Capabilities *lifecycle.CapabilityToken `json:"capabilities,omitempty"` // guest orchestration capabilities
	Kit          string                    `json:"kit,omitempty"`
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
	} else {
		// Auto-create workspace under WorkspacesDir/{handle or id}
		wsName := req.Handle
		if wsName == "" {
			wsName = id
		}
		autoWs := filepath.Join(s.cfg.WorkspacesDir, wsName)
		os.MkdirAll(autoWs, 0755)
		req.Workspace = autoWs
		opts = append(opts, lifecycle.WithWorkspace(autoWs), lifecycle.WithAutoWorkspace())
	}
	if req.MemoryMB > 0 {
		opts = append(opts, lifecycle.WithMemory(req.MemoryMB))
	}
	if req.VCPUs > 0 {
		opts = append(opts, lifecycle.WithVCPUs(req.VCPUs))
	}
	if req.IdlePolicy != "" {
		opts = append(opts, lifecycle.WithIdlePolicy(req.IdlePolicy))
	}
	if req.Capabilities != nil {
		opts = append(opts, lifecycle.WithCapabilities(req.Capabilities))
	}
	if req.Kit != "" {
		opts = append(opts, lifecycle.WithKit(req.Kit))
	}

	// Write default kit configs if they don't exist yet
	if req.Kit != "" {
		s.writeDefaultKitConfigs(req.Kit, req.Workspace, req.Handle, id)
	}

	// Create in lifecycle manager.
	// This triggers onInstanceCreated which handles router port allocation
	// and registry persistence — same path as guest API spawns.
	s.lifecycle.CreateInstance(id, req.Command, nil, opts...)

	// Start instance daemons (e.g., gateway) if kit has instance_daemons
	if req.Kit != "" && s.daemons != nil {
		handle := req.Handle
		if handle == "" {
			handle = id
		}
		if err := s.daemons.StartDaemons(id, handle, req.Kit, env); err != nil {
			log.Printf("instance %s start daemons: %v", id, err)
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
	if req.Kit != "" {
		resp["kit"] = req.Kit
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

	// Start instance daemons on re-enable
	if inst.Kit != "" && s.daemons != nil {
		handle := inst.HandleAlias
		if handle == "" {
			handle = inst.ID
		}
		if err := s.daemons.StartDaemons(inst.ID, handle, inst.Kit, inst.Env); err != nil {
			log.Printf("instance %s start daemons: %v", inst.ID, err)
		}
	}

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
	if inst.Kit != "" {
		resp["kit"] = inst.Kit
		if s.daemons != nil {
			resp["gateway_running"] = s.daemons.IsRunning(inst.ID)
		}
	}
	if inst.WorkspacePath != "" {
		resp["workspace"] = inst.WorkspacePath
	}
	if !inst.StoppedAt.IsZero() {
		resp["stopped_at"] = inst.StoppedAt.Format(time.RFC3339)
	}

	if inst.IdlePolicy != "" {
		resp["idle_policy"] = inst.IdlePolicy
	}
	if held, reason, expiresAt := s.lifecycle.LeaseInfo(inst.ID); held {
		resp["lease_held"] = true
		resp["lease_reason"] = reason
		resp["lease_expires_at"] = expiresAt.Format(time.RFC3339)
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

// instanceSortKey returns a sort key for instance ordering:
// running → paused → stopped → disabled, with disabled always last.
func instanceSortKey(state string, enabled bool) int {
	if !enabled {
		return 99 // disabled always last
	}
	switch state {
	case "running":
		return 0
	case "starting":
		return 1
	case "paused":
		return 2
	default: // stopped
		return 3
	}
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances := s.lifecycle.ListInstances()

	// Stable sort: running → paused → stopped → disabled, then oldest first within each group.
	sort.SliceStable(instances, func(i, j int) bool {
		si := instanceSortKey(instances[i].State, instances[i].Enabled)
		sj := instanceSortKey(instances[j].State, instances[j].Enabled)
		if si != sj {
			return si < sj
		}
		return instances[i].UpdatedAt.Before(instances[j].UpdatedAt)
	})

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
			"updated_at":        inst.UpdatedAt.Format(time.RFC3339),
			"last_active_at":    s.lifecycle.LastActivity(inst.ID).Format(time.RFC3339),
			"active_connections": s.lifecycle.ActiveConns(inst.ID),
		}
		if inst.HandleAlias != "" {
			entry["handle"] = inst.HandleAlias
		}
		if inst.ImageRef != "" {
			entry["image_ref"] = inst.ImageRef
		}
		if inst.Kit != "" {
			entry["kit"] = inst.Kit
		}
		if !inst.StoppedAt.IsZero() {
			entry["stopped_at"] = inst.StoppedAt.Format(time.RFC3339)
		}
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
				entry["endpoints"] = eps
			}
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
	id := s.resolveInstanceID(pathParam(r, "id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if err := s.lifecycle.PauseInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleResumeInstance(w http.ResponseWriter, r *http.Request) {
	id := s.resolveInstanceID(pathParam(r, "id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if err := s.lifecycle.ResumeInstance(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

// resolveInstanceID resolves an ID-or-handle string to the canonical instance ID.
// Returns empty string if not found.
func (s *Server) resolveInstanceID(idOrHandle string) string {
	if inst := s.lifecycle.GetInstance(idOrHandle); inst != nil {
		return inst.ID
	}
	if inst := s.lifecycle.GetInstanceByHandle(idOrHandle); inst != nil {
		return inst.ID
	}
	return ""
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

	// Start instance daemons if kit instance and not already running
	if inst.Kit != "" && s.daemons != nil {
		handle := inst.HandleAlias
		if handle == "" {
			handle = inst.ID
		}
		if err := s.daemons.StartDaemons(inst.ID, handle, inst.Kit, inst.Env); err != nil {
			log.Printf("instance %s start daemons: %v", inst.ID, err)
		}
	}

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

	// Stop instance daemons
	if s.daemons != nil {
		s.daemons.StopDaemons(inst.ID)
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

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	resolvedID := inst.ID

	// Check if auto-workspace should be cleaned up before deleting the instance.
	var autoWsPath string
	if inst.AutoWorkspace && inst.WorkspacePath != "" {
		autoWsPath = inst.WorkspacePath
	}

	// Stop instance daemons
	if s.daemons != nil {
		s.daemons.StopDaemons(resolvedID)
	}

	// Free public port listeners before deleting instance
	if s.router != nil {
		s.router.FreeAllPorts(resolvedID)
	}

	if err := s.lifecycle.DeleteInstance(resolvedID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if s.registry != nil {
		s.registry.DeleteInstance(resolvedID)
	}

	// Delete auto-created workspace (user-provided workspaces are kept)
	if autoWsPath != "" {
		if err := os.RemoveAll(autoWsPath); err != nil {
			log.Printf("delete auto-workspace %s: %v", autoWsPath, err)
		} else {
			log.Printf("deleted auto-workspace %s", autoWsPath)
		}
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

func (s *Server) handleExposePort(w http.ResponseWriter, r *http.Request) {
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

	var req exposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Port <= 0 {
		writeError(w, http.StatusBadRequest, "port (guest_port) is required")
		return
	}

	proto := req.Protocol
	if proto == "" {
		proto = "http"
	}

	publicPort, err := s.lifecycle.ExposePort(inst.ID, req.Port, req.PublicPort, proto)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Persist public port mapping to registry
	if s.registry != nil && s.router != nil {
		publicPorts := make(map[int]int)
		for _, ep := range s.router.GetAllPublicPorts(inst.ID) {
			publicPorts[ep.GuestPort] = ep.PublicPort
		}
		s.registry.UpdatePublicPorts(inst.ID, publicPorts)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"guest_port":  req.Port,
		"public_port": publicPort,
		"protocol":    proto,
		"url":         fmt.Sprintf("http://127.0.0.1:%d", publicPort),
	})
}

func (s *Server) handleUnexposePort(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	guestPortStr := pathParam(r, "guest_port")

	guestPort := 0
	fmt.Sscanf(guestPortStr, "%d", &guestPort)
	if guestPort <= 0 {
		writeError(w, http.StatusBadRequest, "invalid guest_port")
		return
	}

	// Resolve by ID or handle
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if err := s.lifecycle.UnexposePort(inst.ID, guestPort); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Persist public port mapping to registry
	if s.registry != nil && s.router != nil {
		publicPorts := make(map[int]int)
		for _, ep := range s.router.GetAllPublicPorts(inst.ID) {
			publicPorts[ep.GuestPort] = ep.PublicPort
		}
		s.registry.UpdatePublicPorts(inst.ID, publicPorts)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// Workspace file handlers — read/write files in instance workspaces.

const maxWorkspaceFileSize = 10 << 20 // 10MB

// validateWorkspacePath checks that a relative path is safe (no traversal, no absolute).
func validateWorkspacePath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute paths not allowed")
	}
	cleaned := filepath.Clean(p)
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, string(filepath.Separator)+"..") {
		return fmt.Errorf("path traversal not allowed")
	}
	return nil
}

func (s *Server) handleWorkspaceRead(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	relPath := r.URL.Query().Get("path")

	if err := validateWorkspacePath(relPath); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve instance
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if inst.WorkspacePath == "" {
		writeError(w, http.StatusNotFound, "instance has no workspace")
		return
	}

	fullPath := filepath.Join(inst.WorkspacePath, filepath.Clean(relPath))

	// Verify the resolved path is within the workspace (symlink escape check)
	resolved, err := filepath.EvalSymlinks(filepath.Dir(fullPath))
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	wsResolved, _ := filepath.EvalSymlinks(inst.WorkspacePath)
	if !strings.HasPrefix(resolved, wsResolved) {
		writeError(w, http.StatusForbidden, "path escapes workspace")
		return
	}

	fi, err := os.Stat(fullPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if fi.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if fi.Size() > maxWorkspaceFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds 10MB limit")
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read failed")
		return
	}

	// Detect content type from extension, fall back to octet-stream
	ct := mime.TypeByExtension(filepath.Ext(fullPath))
	if ct == "" {
		ct = http.DetectContentType(data)
	}

	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) handleWorkspaceWrite(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	relPath := r.URL.Query().Get("path")

	if err := validateWorkspacePath(relPath); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve instance
	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if inst.WorkspacePath == "" {
		writeError(w, http.StatusNotFound, "instance has no workspace")
		return
	}

	// Read body with size limit
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWorkspaceFileSize+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read body failed")
		return
	}
	if len(data) > maxWorkspaceFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 10MB limit")
		return
	}

	fullPath := filepath.Join(inst.WorkspacePath, filepath.Clean(relPath))

	// Verify the resolved parent is within the workspace
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		writeError(w, http.StatusInternalServerError, "create directory failed")
		return
	}
	resolved, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve path failed")
		return
	}
	wsResolved, _ := filepath.EvalSymlinks(inst.WorkspacePath)
	if !strings.HasPrefix(resolved, wsResolved) {
		writeError(w, http.StatusForbidden, "path escapes workspace")
		return
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "write failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeDefaultKitConfigs writes default config files from the kit manifest
// if they don't already exist. Called on instance creation.
func (s *Server) writeDefaultKitConfigs(kitName, workspacePath, handle, id string) {
	manifest, err := kit.LoadManifest(kitName)
	if err != nil {
		return
	}

	// Workspace configs (guest-side)
	for _, c := range manifest.Config {
		if c.Default == nil || workspacePath == "" {
			continue
		}
		fullPath := filepath.Join(workspacePath, filepath.Clean(c.Path))
		if _, err := os.Stat(fullPath); err == nil {
			continue // already exists
		}
		os.MkdirAll(filepath.Dir(fullPath), 0755)
		formatted, err := json.MarshalIndent(json.RawMessage(c.Default), "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(fullPath, append(formatted, '\n'), 0644)
		log.Printf("kit %s: wrote default config %s", kitName, c.Path)
	}

	// Host configs (daemon-side)
	h := handle
	if h == "" {
		h = id
	}
	for _, d := range manifest.InstanceDaemons {
		if d.Config == nil || d.Config.Default == nil {
			continue
		}
		dir := filepath.Join(kit.KitsDir(), h)
		fullPath := filepath.Join(dir, d.Config.Path)
		if _, err := os.Stat(fullPath); err == nil {
			continue // already exists
		}
		os.MkdirAll(dir, 0755)
		formatted, err := json.MarshalIndent(json.RawMessage(d.Config.Default), "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(fullPath, append(formatted, '\n'), 0644)
		log.Printf("kit %s: wrote default config %s/%s", kitName, h, d.Config.Path)
	}
}

// Kit config handlers — host-side config at ~/.aegis/kits/{handle}/{file}

func (s *Server) kitConfigDir(inst *lifecycle.Instance) string {
	handle := inst.HandleAlias
	if handle == "" {
		handle = inst.ID
	}
	return filepath.Join(kit.KitsDir(), handle)
}

func (s *Server) handleKitConfigRead(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	file := r.URL.Query().Get("file")
	if file == "" || strings.Contains(file, "/") || strings.Contains(file, "..") {
		writeError(w, http.StatusBadRequest, "file parameter required (filename only, no paths)")
		return
	}

	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	fullPath := filepath.Join(s.kitConfigDir(inst), file)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "config file not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) handleKitConfigWrite(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	file := r.URL.Query().Get("file")
	if file == "" || strings.Contains(file, "/") || strings.Contains(file, "..") {
		writeError(w, http.StatusBadRequest, "file parameter required (filename only, no paths)")
		return
	}

	inst := s.lifecycle.GetInstance(id)
	if inst == nil {
		inst = s.lifecycle.GetInstanceByHandle(id)
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxWorkspaceFileSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(data) > maxWorkspaceFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large (10MB limit)")
		return
	}

	dir := s.kitConfigDir(inst)
	os.MkdirAll(dir, 0755)

	if err := os.WriteFile(filepath.Join(dir, file), data, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "write failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Tether handlers — Agent Kit messaging

func (s *Server) handleTetherIngress(w http.ResponseWriter, r *http.Request) {
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

	var frame tether.Frame
	if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid frame: %v", err))
		return
	}

	// Wake-on-message: ensure instance is running before delivering
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.lifecycle.EnsureInstance(ctx, inst.ID); err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("wake failed: %v", err))
		return
	}

	// Assign ingress seq for cursor tracking
	ts := s.lifecycle.TetherStore()
	var ingressSeq int64
	if ts != nil {
		ingressSeq = ts.NextSeq(inst.ID)
		frame.Seq = ingressSeq
	}

	if err := s.lifecycle.SendTetherFrame(inst.ID, frame); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("send frame: %v", err))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"msg_id":      frame.MsgID,
		"session_id":  frame.Session.ID,
		"ingress_seq": ingressSeq,
	})
}

func (s *Server) handleTetherStream(w http.ResponseWriter, r *http.Request) {
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

	ts := s.lifecycle.TetherStore()
	if ts == nil {
		writeError(w, http.StatusServiceUnavailable, "tether not available")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Stream recent frames first
	recent := ts.Recent(inst.ID, 50)
	for _, frame := range recent {
		streamJSON(w, frame)
	}

	// Subscribe to live frames
	ch, unsub := ts.Subscribe(inst.ID)
	defer unsub()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			streamJSON(w, frame)
		}
	}
}

func (s *Server) handleTetherPoll(w http.ResponseWriter, r *http.Request) {
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

	ts := s.lifecycle.TetherStore()
	if ts == nil {
		writeError(w, http.StatusServiceUnavailable, "tether not available")
		return
	}

	q := r.URL.Query()

	opts := tether.QueryOpts{
		Channel:      q.Get("channel"),
		SessionID:    q.Get("session_id"),
		ReplyToMsgID: q.Get("reply_to_msg_id"),
	}
	if v := q.Get("after_seq"); v != "" {
		fmt.Sscanf(v, "%d", &opts.AfterSeq)
	}
	if v := q.Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &opts.Limit)
	}
	if v := q.Get("types"); v != "" {
		opts.Types = strings.Split(v, ",")
	}

	waitMs := 0
	if v := q.Get("wait_ms"); v != "" {
		fmt.Sscanf(v, "%d", &waitMs)
	}
	if waitMs > 30000 {
		waitMs = 30000
	}

	var result tether.QueryResult
	if waitMs > 0 {
		result = ts.WaitForFrames(r.Context(), inst.ID, opts, time.Duration(waitMs)*time.Millisecond)
	} else {
		result = ts.Query(inst.ID, opts)
	}

	writeJSON(w, http.StatusOK, result)
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

func (s *Server) handleGetSecret(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	sec, err := s.registry.GetSecretByName(name)
	if err != nil || sec == nil {
		writeError(w, http.StatusNotFound, "secret not found")
		return
	}
	val, err := s.secretStore.DecryptString(sec.EncryptedValue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "decrypt failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "value": val})
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
