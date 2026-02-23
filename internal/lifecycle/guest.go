package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/xfeldman/aegisvm/internal/vmm"
)

// handleGuestRequest processes RPC requests from the harness's guest API server.
// These are requests from guest processes (e.g., OpenClaw bot spawning work instances).
func (m *Manager) handleGuestRequest(inst *Instance, method string, params json.RawMessage) (interface{}, error) {
	// Methods that don't require spawn capabilities
	switch method {
	case "guest.self_info":
		return m.guestSelfInfo(inst)
	case "guest.list_children":
		return m.guestListChildren(inst)
	case "guest.expose_port":
		return m.guestExposePort(inst, params)
	case "guest.unexpose_port":
		return m.guestUnexposePort(inst, params)
	}

	// Methods that require a valid capability token
	var baseParams struct {
		Token string `json:"_token"`
	}
	json.Unmarshal(params, &baseParams)

	if m.secretStore == nil {
		return nil, fmt.Errorf("guest orchestration not available (no secret store)")
	}

	token, err := ValidateToken(m.secretStore, baseParams.Token)
	if err != nil {
		return nil, fmt.Errorf("invalid capability token: %w", err)
	}

	if token.ParentID != inst.ID {
		return nil, fmt.Errorf("token parent_id mismatch")
	}

	inst.mu.Lock()
	state := inst.State
	inst.mu.Unlock()
	if state != StateRunning {
		return nil, fmt.Errorf("parent instance not running (state=%s)", state)
	}

	switch method {
	case "guest.spawn":
		return m.guestSpawn(inst, token, params)
	case "guest.stop_child":
		return m.guestStopChild(inst, params)
	default:
		return nil, fmt.Errorf("unknown guest method: %s", method)
	}
}

// guestSpawn creates a child instance.
func (m *Manager) guestSpawn(parent *Instance, token *CapabilityToken, params json.RawMessage) (interface{}, error) {
	if !token.Spawn {
		return nil, fmt.Errorf("spawn not permitted by capability token")
	}
	if token.SpawnDepth <= 0 {
		return nil, fmt.Errorf("spawn depth exhausted")
	}

	var req struct {
		Handle    string            `json:"handle"`
		Command   []string          `json:"command"`
		ImageRef  string            `json:"image_ref"`
		Workspace string            `json:"workspace"`
		Exposes   []int             `json:"exposes"`
		Secrets   []string          `json:"secrets"`
		MemoryMB  int               `json:"memory_mb"`
		VCPUs     int               `json:"vcpus"`
		Env       map[string]string `json:"env"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid spawn params: %w", err)
	}

	if len(req.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}

	// Enforce capability ceilings
	if req.MemoryMB > token.MaxMemoryMB && token.MaxMemoryMB > 0 {
		return nil, fmt.Errorf("memory_mb %d exceeds cap %d", req.MemoryMB, token.MaxMemoryMB)
	}
	if req.VCPUs > token.MaxVCPUs && token.MaxVCPUs > 0 {
		return nil, fmt.Errorf("vcpus %d exceeds cap %d", req.VCPUs, token.MaxVCPUs)
	}
	if len(req.Exposes) > token.MaxExposePorts && token.MaxExposePorts > 0 {
		return nil, fmt.Errorf("expose_ports count %d exceeds cap %d", len(req.Exposes), token.MaxExposePorts)
	}

	// Check image allowlist
	if len(token.AllowedImages) > 0 && req.ImageRef != "" {
		allowed := false
		for _, img := range token.AllowedImages {
			if img == req.ImageRef || img == "*" {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("image %q not in allowed list", req.ImageRef)
		}
	}

	// Check max children
	if token.MaxChildren > 0 {
		m.mu.Lock()
		childCount := 0
		for _, inst := range m.instances {
			if inst.ParentID == parent.ID {
				childCount++
			}
		}
		m.mu.Unlock()
		if childCount >= token.MaxChildren {
			return nil, fmt.Errorf("max children reached (%d)", token.MaxChildren)
		}
	}

	// Create the child instance
	id := fmt.Sprintf("inst-%d", time.Now().UnixNano())

	var exposePorts []vmm.PortExpose
	for _, p := range req.Exposes {
		exposePorts = append(exposePorts, vmm.PortExpose{GuestPort: p, Protocol: "http"})
	}

	// Build child's capability token (intersection of parent's caps)
	childCaps := &CapabilityToken{
		Spawn:          token.SpawnDepth > 1,
		SpawnDepth:     token.SpawnDepth - 1,
		MaxChildren:    token.MaxChildren,
		AllowedImages:  token.AllowedImages,
		MaxMemoryMB:    token.MaxMemoryMB,
		MaxVCPUs:       token.MaxVCPUs,
		AllowedSecrets: token.AllowedSecrets,
		MaxExposePorts: token.MaxExposePorts,
	}

	opts := []InstanceOption{
		WithParentID(parent.ID),
		WithCapabilities(childCaps),
	}
	if req.Handle != "" {
		opts = append(opts, WithHandle(req.Handle))
	}
	if req.ImageRef != "" {
		opts = append(opts, WithImageRef(req.ImageRef))
	}
	if req.Workspace != "" {
		// Resolve guest workspace path to host path.
		// The guest sees /workspace/... but the host needs the actual directory.
		// If the parent has a workspace mounted, translate /workspace/subpath
		// to parentHostPath/subpath. Otherwise pass through as-is (host path).
		workspace := req.Workspace
		if parent.WorkspacePath != "" && strings.HasPrefix(workspace, "/workspace") {
			// /workspace → parent's host path
			// /workspace/subdir → parent's host path + /subdir
			suffix := strings.TrimPrefix(workspace, "/workspace")
			workspace = parent.WorkspacePath + suffix
		}
		opts = append(opts, WithWorkspace(workspace))
	}
	if len(req.Env) > 0 {
		opts = append(opts, WithEnv(req.Env))
	}
	if req.MemoryMB > 0 {
		opts = append(opts, WithMemory(req.MemoryMB))
	}
	if req.VCPUs > 0 {
		opts = append(opts, WithVCPUs(req.VCPUs))
	}

	inst := m.CreateInstance(id, req.Command, exposePorts, opts...)

	log.Printf("guest.spawn: parent=%s child=%s image=%s cmd=%v", parent.ID, id, req.ImageRef, req.Command)

	// Boot in background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := m.bootInstance(ctx, inst); err != nil {
			log.Printf("guest.spawn: boot %s failed: %v", id, err)
		}
	}()

	return map[string]interface{}{
		"id":        inst.ID,
		"handle":    inst.HandleAlias,
		"state":     "starting",
		"parent_id": parent.ID,
	}, nil
}

// guestListChildren returns child instances of the given parent.
func (m *Manager) guestListChildren(parent *Instance) (interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var children []map[string]interface{}
	for _, inst := range m.instances {
		if inst.ParentID == parent.ID {
			inst.mu.Lock()
			state := inst.State
			inst.mu.Unlock()

			child := map[string]interface{}{
				"id":     inst.ID,
				"handle": inst.HandleAlias,
				"state":  state,
			}

			// Use router public ports (not VMM backend ports)
			publicPorts := m.GetPublicPorts(inst.ID)
			if len(publicPorts) > 0 {
				var eps []map[string]interface{}
				for guestPort, publicPort := range publicPorts {
					eps = append(eps, map[string]interface{}{
						"guest_port":  guestPort,
						"public_port": publicPort,
						"url":         fmt.Sprintf("http://127.0.0.1:%d", publicPort),
					})
				}
				child["endpoints"] = eps
			}

			children = append(children, child)
		}
	}

	if children == nil {
		children = []map[string]interface{}{}
	}
	return children, nil
}

// guestStopChild stops a child instance.
func (m *Manager) guestStopChild(parent *Instance, params json.RawMessage) (interface{}, error) {
	var req struct {
		ChildID string `json:"child_id"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	child := m.GetInstance(req.ChildID)
	if child == nil {
		return nil, fmt.Errorf("child instance not found: %s", req.ChildID)
	}
	if child.ParentID != parent.ID {
		return nil, fmt.Errorf("instance %s is not a child of %s", req.ChildID, parent.ID)
	}

	m.StopInstance(req.ChildID)
	return map[string]string{"status": "stopped"}, nil
}

// guestSelfInfo returns info about the calling instance.
func (m *Manager) guestSelfInfo(inst *Instance) (interface{}, error) {
	inst.mu.Lock()
	state := inst.State
	inst.mu.Unlock()

	info := map[string]interface{}{
		"id":       inst.ID,
		"handle":   inst.HandleAlias,
		"state":    state,
		"image":    inst.ImageRef,
		"parent_id": inst.ParentID,
	}

	publicPorts := m.GetPublicPorts(inst.ID)
	if len(publicPorts) > 0 {
		var endpoints []map[string]interface{}
		for guestPort, publicPort := range publicPorts {
			endpoints = append(endpoints, map[string]interface{}{
				"guest_port":  guestPort,
				"public_port": publicPort,
				"url":         fmt.Sprintf("http://127.0.0.1:%d", publicPort),
			})
		}
		info["endpoints"] = endpoints
	}

	return info, nil
}

// guestExposePort exposes a port from inside the VM (self-operation, no token needed).
func (m *Manager) guestExposePort(inst *Instance, params json.RawMessage) (interface{}, error) {
	var req struct {
		GuestPort  int    `json:"guest_port"`
		PublicPort int    `json:"public_port"`
		Protocol   string `json:"protocol"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.GuestPort <= 0 {
		return nil, fmt.Errorf("guest_port is required")
	}

	publicPort, err := m.ExposePort(inst.ID, req.GuestPort, req.PublicPort, req.Protocol)
	if err != nil {
		return nil, err
	}

	protocol := req.Protocol
	if protocol == "" {
		protocol = "http"
	}
	return map[string]interface{}{
		"guest_port":  req.GuestPort,
		"public_port": publicPort,
		"protocol":    protocol,
		"url":         fmt.Sprintf("http://127.0.0.1:%d", publicPort),
	}, nil
}

// guestUnexposePort unexposes a port from inside the VM (self-operation, no token needed).
func (m *Manager) guestUnexposePort(inst *Instance, params json.RawMessage) (interface{}, error) {
	var req struct {
		GuestPort int `json:"guest_port"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.GuestPort <= 0 {
		return nil, fmt.Errorf("guest_port is required")
	}

	if err := m.UnexposePort(inst.ID, req.GuestPort); err != nil {
		return nil, err
	}

	return map[string]string{"status": "ok"}, nil
}

// CascadeStopChildren stops all children of a parent instance.
// Called when a parent instance stops.
func (m *Manager) CascadeStopChildren(parentID string) {
	m.mu.Lock()
	var childIDs []string
	for _, inst := range m.instances {
		if inst.ParentID == parentID {
			childIDs = append(childIDs, inst.ID)
		}
	}
	m.mu.Unlock()

	for _, id := range childIDs {
		log.Printf("cascade stop: parent=%s child=%s", parentID, id)
		m.StopInstance(id)
	}
}
