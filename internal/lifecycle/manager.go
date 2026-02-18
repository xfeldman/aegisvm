// Package lifecycle manages the state machine for serve-mode instances.
//
// State transitions:
//
//	STOPPED → STARTING → RUNNING ⇄ PAUSED → STOPPED
//
// RUNNING → PAUSED after idle timeout (SIGSTOP).
// PAUSED → RUNNING on next request (SIGCONT).
// PAUSED → STOPPED after extended idle (StopVM, resources freed, can reboot on next request).
//
// STOPPED is the only non-running terminal state. Explicit user stop
// (StopInstance) removes the instance from the map entirely.
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Instance states
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StatePaused   = "paused"
)

// Instance represents a managed serve-mode instance.
type Instance struct {
	mu sync.Mutex

	ID          string
	State       string
	Command     []string
	ExposePorts []vmm.PortExpose
	Handle      vmm.Handle
	Channel     vmm.ControlChannel
	Endpoints   []vmm.HostEndpoint

	// App/release association (M2+)
	AppID         string
	ReleaseID     string
	RootfsPath    string // if set, used instead of cfg.BaseRootfsPath
	WorkspacePath string // if set, passed to VMConfig.WorkspacePath

	// Connection tracking
	activeConns int

	// Idle management
	idleTimer      *time.Timer
	terminateTimer *time.Timer
	lastActivity   time.Time
}

// FirstGuestPort returns the first exposed guest port, or 0 if none.
func (inst *Instance) FirstGuestPort() int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.ExposePorts) > 0 {
		return inst.ExposePorts[0].GuestPort
	}
	return 0
}

// Manager owns instances and drives their lifecycle.
type Manager struct {
	mu        sync.Mutex
	instances map[string]*Instance
	vmm       vmm.VMM
	cfg       *config.Config

	// Callbacks
	onStateChange func(id, state string)
}

// NewManager creates a lifecycle manager.
func NewManager(v vmm.VMM, cfg *config.Config) *Manager {
	return &Manager{
		instances: make(map[string]*Instance),
		vmm:       v,
		cfg:       cfg,
	}
}

// OnStateChange registers a callback for state changes (e.g., to persist to registry).
func (m *Manager) OnStateChange(fn func(id, state string)) {
	m.onStateChange = fn
}

// InstanceOption configures an instance at creation time.
type InstanceOption func(*Instance)

// WithApp sets the app and release association.
func WithApp(appID, releaseID string) InstanceOption {
	return func(inst *Instance) {
		inst.AppID = appID
		inst.ReleaseID = releaseID
	}
}

// WithRootfs sets a custom rootfs path (instead of the default base rootfs).
func WithRootfs(path string) InstanceOption {
	return func(inst *Instance) {
		inst.RootfsPath = path
	}
}

// WithWorkspace sets a workspace volume path.
func WithWorkspace(path string) InstanceOption {
	return func(inst *Instance) {
		inst.WorkspacePath = path
	}
}

// CreateInstance creates a new instance definition without starting it.
func (m *Manager) CreateInstance(id string, command []string, exposePorts []vmm.PortExpose, opts ...InstanceOption) *Instance {
	inst := &Instance{
		ID:          id,
		State:       StateStopped,
		Command:     command,
		ExposePorts: exposePorts,
	}
	for _, opt := range opts {
		opt(inst)
	}

	m.mu.Lock()
	m.instances[id] = inst
	m.mu.Unlock()

	return inst
}

// EnsureInstance ensures an instance is running. This is the single entry point
// for the router — it never needs to know the current state.
//
//   - stopped → boot (cold start, ~1-2s)
//   - paused → SIGCONT resume (<100ms)
//   - starting → block until running or failed (with ctx timeout)
//   - running → noop
//
// The router calls EnsureInstance on every request. If the instance is booting,
// EnsureInstance blocks. The router's request context has a 30s timeout, so if
// boot takes too long the request fails with 503 and the loading page is shown.
func (m *Manager) EnsureInstance(ctx context.Context, id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	state := inst.State
	inst.mu.Unlock()

	switch state {
	case StateRunning:
		return nil
	case StatePaused:
		return m.resumeInstance(inst)
	case StateStopped:
		return m.bootInstance(ctx, inst)
	case StateStarting:
		return m.waitForRunning(ctx, inst)
	default:
		return fmt.Errorf("instance %s in unexpected state: %s", id, state)
	}
}

func (m *Manager) bootInstance(ctx context.Context, inst *Instance) error {
	inst.mu.Lock()
	if inst.State != StateStopped {
		inst.mu.Unlock()
		return nil
	}
	inst.State = StateStarting
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateStarting)

	rootfsPath := m.cfg.BaseRootfsPath
	if inst.RootfsPath != "" {
		rootfsPath = inst.RootfsPath
	}

	vmCfg := vmm.VMConfig{
		Rootfs: vmm.RootFS{
			Type: m.vmm.Capabilities().RootFSType,
			Path: rootfsPath,
		},
		MemoryMB:      m.cfg.DefaultMemoryMB,
		VCPUs:         m.cfg.DefaultVCPUs,
		ExposePorts:   inst.ExposePorts,
		WorkspacePath: inst.WorkspacePath,
	}

	handle, err := m.vmm.CreateVM(vmCfg)
	if err != nil {
		inst.mu.Lock()
		inst.State = StateStopped
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("create VM: %w", err)
	}

	ch, err := m.vmm.StartVM(handle)
	if err != nil {
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("start VM: %w", err)
	}

	endpoints, _ := m.vmm.HostEndpoints(handle)

	// Send startServer RPC to harness
	rpcReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "startServer",
		"params": map[string]interface{}{
			"command":        inst.Command,
			"readiness_port": inst.ExposePorts[0].GuestPort,
		},
		"id": 1,
	})

	rpcCtx, rpcCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rpcCancel()

	if err := ch.Send(rpcCtx, rpcReq); err != nil {
		ch.Close()
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("send startServer RPC: %w", err)
	}

	// Read startServer response
	msg, err := ch.Recv(rpcCtx)
	if err != nil {
		ch.Close()
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("recv startServer response: %w", err)
	}

	var resp map[string]interface{}
	json.Unmarshal(msg, &resp)
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		errMsg, _ := errObj["message"].(string)
		ch.Close()
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("startServer failed: %s", errMsg)
	}

	// Wait for serverReady notification
	readyCtx, readyCancel := context.WithTimeout(ctx, 60*time.Second)
	defer readyCancel()

	for {
		msg, err := ch.Recv(readyCtx)
		if err != nil {
			ch.Close()
			m.vmm.StopVM(handle)
			inst.mu.Lock()
			inst.State = StateStopped
			inst.mu.Unlock()
			m.notifyStateChange(inst.ID, StateStopped)
			return fmt.Errorf("waiting for serverReady: %w", err)
		}

		var notif map[string]interface{}
		json.Unmarshal(msg, &notif)

		method, _ := notif["method"].(string)
		switch method {
		case "serverReady":
			// Server is ready
			inst.mu.Lock()
			inst.Handle = handle
			inst.Channel = ch
			inst.Endpoints = endpoints
			inst.State = StateRunning
			inst.lastActivity = time.Now()
			inst.mu.Unlock()
			m.notifyStateChange(inst.ID, StateRunning)
			m.startIdleTimer(inst)
			log.Printf("instance %s: server ready (endpoints: %v)", inst.ID, endpoints)
			return nil
		case "serverFailed":
			params, _ := notif["params"].(map[string]interface{})
			errMsg, _ := params["error"].(string)
			ch.Close()
			m.vmm.StopVM(handle)
			inst.mu.Lock()
			inst.State = StateStopped
			inst.mu.Unlock()
			m.notifyStateChange(inst.ID, StateStopped)
			return fmt.Errorf("server failed to start: %s", errMsg)
		case "log":
			// Stream logs during startup — just continue
			continue
		}
	}
}

func (m *Manager) resumeInstance(inst *Instance) error {
	inst.mu.Lock()
	if inst.State != StatePaused {
		inst.mu.Unlock()
		return nil
	}
	handle := inst.Handle
	inst.mu.Unlock()

	log.Printf("instance %s: resuming (SIGCONT)", inst.ID)
	if err := m.vmm.ResumeVM(handle); err != nil {
		return fmt.Errorf("resume VM: %w", err)
	}

	inst.mu.Lock()
	inst.State = StateRunning
	inst.lastActivity = time.Now()
	if inst.terminateTimer != nil {
		inst.terminateTimer.Stop()
		inst.terminateTimer = nil
	}
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateRunning)
	m.startIdleTimer(inst)
	return nil
}

func (m *Manager) waitForRunning(ctx context.Context, inst *Instance) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			inst.mu.Lock()
			state := inst.State
			inst.mu.Unlock()
			switch state {
			case StateRunning:
				return nil
			case StateStopped:
				return fmt.Errorf("instance %s failed to start", inst.ID)
			}
		}
	}
}

// ResetActivity resets the idle timer. Called by the router on each connection.
func (m *Manager) ResetActivity(id string) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return
	}

	inst.mu.Lock()
	inst.lastActivity = time.Now()
	inst.activeConns++
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	inst.mu.Unlock()
}

// OnConnectionClose decrements active connections and may start idle timer.
func (m *Manager) OnConnectionClose(id string) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return
	}

	inst.mu.Lock()
	inst.activeConns--
	if inst.activeConns < 0 {
		inst.activeConns = 0
	}
	conns := inst.activeConns
	inst.lastActivity = time.Now()
	inst.mu.Unlock()

	if conns == 0 {
		m.startIdleTimer(inst)
	}
}

func (m *Manager) startIdleTimer(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
	}

	inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
		m.pauseInstance(inst)
	})
}

func (m *Manager) pauseInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StateRunning || inst.activeConns > 0 {
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	inst.mu.Unlock()

	log.Printf("instance %s: pausing (idle timeout)", inst.ID)
	if err := m.vmm.PauseVM(handle); err != nil {
		log.Printf("instance %s: pause failed: %v", inst.ID, err)
		return
	}

	inst.mu.Lock()
	inst.State = StatePaused
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StatePaused)

	// Start terminate timer
	inst.mu.Lock()
	inst.terminateTimer = time.AfterFunc(m.cfg.TerminateAfterIdle, func() {
		m.terminateInstance(inst)
	})
	inst.mu.Unlock()
}

func (m *Manager) terminateInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StatePaused {
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	ch := inst.Channel
	inst.State = StateStopped
	inst.Channel = nil
	inst.Endpoints = nil
	inst.mu.Unlock()

	log.Printf("instance %s: stopped (extended idle)", inst.ID)
	m.notifyStateChange(inst.ID, StateStopped)

	if ch != nil {
		ch.Close()
	}
	m.vmm.StopVM(handle)
}

// GetEndpoint returns the host endpoint for a guest port on the given instance.
func (m *Manager) GetEndpoint(id string, guestPort int) (string, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()

	for _, ep := range inst.Endpoints {
		if ep.GuestPort == guestPort {
			return fmt.Sprintf("127.0.0.1:%d", ep.HostPort), nil
		}
	}
	return "", fmt.Errorf("no endpoint for guest port %d on instance %s", guestPort, id)
}

// GetInstance returns the instance state (for API responses).
func (m *Manager) GetInstance(id string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[id]
}

// GetInstanceByApp returns the instance associated with an app ID, or nil.
func (m *Manager) GetInstanceByApp(appID string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst.AppID == appID {
			return inst
		}
	}
	return nil
}

// GetDefaultInstance returns the first instance (for M1 single-instance routing).
func (m *Manager) GetDefaultInstance() *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		return inst
	}
	return nil
}

// StopInstance stops an instance immediately.
func (m *Manager) StopInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	ch := inst.Channel

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
	}
	if inst.terminateTimer != nil {
		inst.terminateTimer.Stop()
	}
	inst.State = StateStopped
	inst.mu.Unlock()
	m.notifyStateChange(id, StateStopped)

	if state == StatePaused {
		// Resume before stopping so the process can be killed cleanly
		m.vmm.ResumeVM(handle)
	}

	// Send shutdown RPC if channel available
	if ch != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownReq, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "shutdown",
			"params":  nil,
			"id":      99,
		})
		ch.Send(shutCtx, shutdownReq)
		cancel()
		ch.Close()
	}

	if state != StateStopped {
		m.vmm.StopVM(handle)
	}

	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()

	return nil
}

// Shutdown stops all instances.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.StopInstance(id); err != nil {
			log.Printf("stop instance %s: %v", id, err)
		}
	}
}

func (m *Manager) notifyStateChange(id, state string) {
	if m.onStateChange != nil {
		m.onStateChange(id, state)
	}
}
