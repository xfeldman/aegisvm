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
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/image"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/overlay"
	"github.com/xfeldman/aegisvm/internal/vmm"
)

// ErrInstanceStopped is returned when exec is attempted on a stopped instance.
var ErrInstanceStopped = errors.New("instance is stopped")

// ErrInstanceDisabled is returned when an operation is attempted on a disabled instance.
var ErrInstanceDisabled = errors.New("instance is disabled")

var rpcIDCounter int64

// Instance states
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StatePaused   = "paused"
)

// Instance represents a managed instance.
type Instance struct {
	mu sync.Mutex

	ID          string
	State       string
	Enabled     bool // policy flag: true = wake-on-connect allowed, false = unreachable
	Command     []string
	ExposePorts []vmm.PortExpose
	Handle      vmm.Handle
	Channel     vmm.ControlChannel
	Endpoints   []vmm.HostEndpoint

	// HandleAlias is a user-friendly name for the instance.
	HandleAlias string

	// ImageRef is the OCI image reference (e.g. "python:3.12-alpine").
	ImageRef string

	RootfsPath    string // if set, used instead of cfg.BaseRootfsPath
	WorkspacePath string // if set, passed to VMConfig.WorkspacePath

	// Resource overrides (0 = use global default)
	MemoryMB int
	VCPUs    int

	// Env holds environment variables to inject (including decrypted secrets).
	Env map[string]string

	// Connection tracking
	activeConns int

	// Idle management
	idleTimer *time.Timer
	stopTimer *time.Timer

	lastActivity time.Time

	// Demuxer for persistent channel Recv loop (nil when stopped)
	demuxer    *channelDemuxer
	logCapture bool // guard against double-attach

	// Exec completion tracking: execID → channel that receives exit code
	execWaiters map[string]chan int

	// Timestamps
	CreatedAt time.Time
	StoppedAt time.Time // zero if never stopped or currently running
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
	logStore  *logstore.Store

	imageCache *image.Cache
	overlay    overlay.Overlay

	// Callbacks
	onStateChange func(id, state string)
}

// NewManager creates a lifecycle manager.
func NewManager(v vmm.VMM, cfg *config.Config, ls *logstore.Store, imgCache *image.Cache, ov overlay.Overlay) *Manager {
	return &Manager{
		instances:  make(map[string]*Instance),
		vmm:        v,
		cfg:        cfg,
		logStore:   ls,
		imageCache: imgCache,
		overlay:    ov,
	}
}

// OnStateChange registers a callback for state changes (e.g., to persist to registry).
func (m *Manager) OnStateChange(fn func(id, state string)) {
	m.onStateChange = fn
}

// InstanceOption configures an instance at creation time.
type InstanceOption func(*Instance)

// WithHandle sets a user-friendly handle alias.
func WithHandle(h string) InstanceOption {
	return func(inst *Instance) {
		inst.HandleAlias = h
	}
}

// WithImageRef sets the OCI image reference.
func WithImageRef(ref string) InstanceOption {
	return func(inst *Instance) {
		inst.ImageRef = ref
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

// WithEnv sets environment variables to inject into the VM.
func WithEnv(env map[string]string) InstanceOption {
	return func(inst *Instance) {
		inst.Env = env
	}
}

// WithMemory sets the VM memory in megabytes (0 = use global default).
func WithMemory(mb int) InstanceOption {
	return func(inst *Instance) {
		inst.MemoryMB = mb
	}
}

// WithVCPUs sets the number of virtual CPUs (0 = use global default).
func WithVCPUs(n int) InstanceOption {
	return func(inst *Instance) {
		inst.VCPUs = n
	}
}

// WithEnabled sets the enabled policy flag.
func WithEnabled(enabled bool) InstanceOption {
	return func(inst *Instance) {
		inst.Enabled = enabled
	}
}

// CreateInstance creates a new instance definition without starting it.
func (m *Manager) CreateInstance(id string, command []string, exposePorts []vmm.PortExpose, opts ...InstanceOption) *Instance {
	inst := &Instance{
		ID:          id,
		State:       StateStopped,
		Enabled:     true,
		Command:     command,
		ExposePorts: exposePorts,
		CreatedAt:   time.Now(),
	}
	for _, opt := range opts {
		opt(inst)
	}

	m.mu.Lock()
	m.instances[id] = inst
	m.mu.Unlock()

	// Pre-create logstore entry so logs are available immediately
	// (before boot goroutine starts).
	m.logStore.GetOrCreate(id)

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
	enabled := inst.Enabled
	state := inst.State
	inst.mu.Unlock()

	if !enabled {
		return ErrInstanceDisabled
	}

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
	inst.StoppedAt = time.Time{} // clear
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateStarting)

	// If image ref is set but no rootfs yet, prepare it
	if inst.ImageRef != "" && inst.RootfsPath == "" {
		rootfs, err := m.prepareImageRootfs(ctx, inst)
		if err != nil {
			inst.mu.Lock()
			inst.State = StateStopped
		inst.StoppedAt = time.Now()
			inst.mu.Unlock()
			m.notifyStateChange(inst.ID, StateStopped)
			return fmt.Errorf("prepare image rootfs: %w", err)
		}
		inst.RootfsPath = rootfs
	}

	rootfsPath := m.cfg.BaseRootfsPath
	if inst.RootfsPath != "" {
		rootfsPath = inst.RootfsPath
	}

	memoryMB := m.cfg.DefaultMemoryMB
	if inst.MemoryMB > 0 {
		memoryMB = inst.MemoryMB
	}
	vcpus := m.cfg.DefaultVCPUs
	if inst.VCPUs > 0 {
		vcpus = inst.VCPUs
	}

	vmCfg := vmm.VMConfig{
		Rootfs: vmm.RootFS{
			Type: m.vmm.Capabilities().RootFSType,
			Path: rootfsPath,
		},
		MemoryMB:      memoryMB,
		VCPUs:         vcpus,
		ExposePorts:   inst.ExposePorts,
		WorkspacePath: inst.WorkspacePath,
	}

	handle, err := m.vmm.CreateVM(vmCfg)
	if err != nil {
		inst.mu.Lock()
		inst.State = StateStopped
		inst.StoppedAt = time.Now()
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("create VM: %w", err)
	}

	ch, err := m.vmm.StartVM(handle)
	if err != nil {
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.StoppedAt = time.Now()
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("start VM: %w", err)
	}

	endpoints, _ := m.vmm.HostEndpoints(handle)

	// Create log store entry for this instance
	il := m.logStore.GetOrCreate(inst.ID)

	// Create demuxer with notification handler
	demux := newChannelDemuxer(ch, func(method string, params json.RawMessage) {
		switch method {
		case "log":
			var lp struct {
				Stream string `json:"stream"`
				Line   string `json:"line"`
				ExecID string `json:"exec_id,omitempty"`
			}
			if json.Unmarshal(params, &lp) == nil {
				source := logstore.SourceServer
				if lp.ExecID != "" {
					source = logstore.SourceExec
				}
				il.Append(lp.Stream, lp.Line, lp.ExecID, source)
			}
		case "processExited":
			var pe struct {
				ExitCode int `json:"exit_code"`
			}
			if json.Unmarshal(params, &pe) == nil {
				go m.handleProcessExited(inst, pe.ExitCode)
			}
		case "execDone":
			var ep struct {
				ExecID   string `json:"exec_id"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(params, &ep) == nil {
				log.Printf("instance %s: exec %s done (exit_code=%d)", inst.ID, ep.ExecID, ep.ExitCode)
				inst.mu.Lock()
				if ch, ok := inst.execWaiters[ep.ExecID]; ok {
					ch <- ep.ExitCode
					close(ch)
					delete(inst.execWaiters, ep.ExecID)
				}
				inst.mu.Unlock()
			}
		}
	})

	// Send run RPC via demuxer
	rpcCtx, rpcCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rpcCancel()

	rpcParams := map[string]interface{}{
		"command": inst.Command,
	}
	if len(inst.Env) > 0 {
		rpcParams["env"] = inst.Env
	}

	resp, err := demux.Call(rpcCtx, "run", rpcParams, nextRPCID())
	if err != nil {
		demux.Stop()
		ch.Close()
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.StoppedAt = time.Now()
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("run RPC: %w", err)
	}

	// Check for error in run response
	var respObj struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &respObj) == nil && respObj.Error != nil {
		demux.Stop()
		ch.Close()
		m.vmm.StopVM(handle)
		inst.mu.Lock()
		inst.State = StateStopped
		inst.StoppedAt = time.Now()
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return fmt.Errorf("run failed: %s", respObj.Error.Message)
	}

	// Instance is RUNNING immediately after run RPC succeeds (no readiness wait)
	inst.mu.Lock()
	inst.Handle = handle
	inst.Channel = ch
	inst.Endpoints = endpoints
	inst.State = StateRunning
	inst.lastActivity = time.Now()
	inst.demuxer = demux
	inst.logCapture = true
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateRunning)
	m.startIdleTimer(inst)
	log.Printf("instance %s: running (endpoints: %v)", inst.ID, endpoints)
	return nil
}

// handleProcessExited handles the processExited notification from the harness.
// This is the primary process exit path — distinct from idle timeout (stopIdleInstance)
// and explicit user stop (StopInstance).
func (m *Manager) handleProcessExited(inst *Instance, exitCode int) {
	inst.mu.Lock()
	if inst.State != StateRunning && inst.State != StateStarting {
		// Already stopped/paused by another path — nothing to do.
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	demux := inst.demuxer

	// Cancel timers
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}

	// Close exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.State = StateStopped
		inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	log.Printf("instance %s: process exited (code=%d)", inst.ID, exitCode)

	il := m.logStore.GetOrCreate(inst.ID)
	il.Append("stdout", fmt.Sprintf("process exited (code=%d)", exitCode), "", logstore.SourceSystem)

	m.notifyStateChange(inst.ID, StateStopped)

	// Shutdown demuxer → VM (demuxer.Stop closes the channel)
	if demux != nil {
		demux.Stop()
	}
	m.vmm.StopVM(handle)
	// Instance stays in the map with logs — removed only by explicit DELETE.
}

func nextRPCID() int64 {
	return atomic.AddInt64(&rpcIDCounter, 1)
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
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
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
	inst.stopTimer = time.AfterFunc(m.cfg.StopAfterIdle, func() {
		m.stopIdleInstance(inst)
	})
	inst.mu.Unlock()
}

func (m *Manager) stopIdleInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StatePaused {
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	ch := inst.Channel
	demux := inst.demuxer
	inst.State = StateStopped
		inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	log.Printf("instance %s: stopped (extended idle)", inst.ID)
	m.notifyStateChange(inst.ID, StateStopped)

	// Stop demuxer (closes channel internally) or close channel directly
	if demux != nil {
		demux.Stop()
	} else if ch != nil {
		ch.Close()
	}
	m.vmm.StopVM(handle)
	// Note: logs are NOT removed here — terminated instances keep logs
	// until explicit deletion via StopInstance or Shutdown.
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

// GetInstanceByHandle returns the instance with the given handle alias, or nil.
func (m *Manager) GetInstanceByHandle(handle string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst.HandleAlias == handle {
			return inst
		}
	}
	return nil
}

// GetDefaultInstance returns the first instance (for single-instance routing).
func (m *Manager) GetDefaultInstance() *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		return inst
	}
	return nil
}

// prepareImageRootfs pulls an OCI image, creates an overlay, and injects the harness.
func (m *Manager) prepareImageRootfs(ctx context.Context, inst *Instance) (string, error) {
	if m.imageCache == nil || m.overlay == nil {
		return "", fmt.Errorf("image cache or overlay not configured")
	}

	cachedDir, _, err := m.imageCache.GetOrPull(ctx, inst.ImageRef)
	if err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}

	overlayID := inst.ID
	overlayDir, err := m.overlay.Create(ctx, cachedDir, overlayID)
	if err != nil {
		return "", fmt.Errorf("create overlay: %w", err)
	}

	harnessBin := filepath.Join(m.cfg.BinDir, "aegis-harness")
	if err := image.InjectHarness(overlayDir, harnessBin); err != nil {
		m.overlay.Remove(overlayID)
		return "", fmt.Errorf("inject harness: %w", err)
	}

	return overlayDir, nil
}

// InstanceCount returns the number of active instances.
func (m *Manager) InstanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

// PauseInstance explicitly pauses a running instance.
func (m *Manager) PauseInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	m.pauseInstance(inst)
	return nil
}

// ResumeInstance explicitly resumes a paused instance.
func (m *Manager) ResumeInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	return m.resumeInstance(inst)
}

// StopInstance stops an instance's VM but keeps it in the map with state STOPPED.
// Logs are preserved. Use DeleteInstance to remove entirely.
func (m *Manager) StopInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	state := inst.State
	if state == StateStopped {
		inst.mu.Unlock()
		return nil // already stopped
	}
	handle := inst.Handle
	demux := inst.demuxer

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}

	// Close all exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.State = StateStopped
		inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()
	m.notifyStateChange(id, StateStopped)

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	m.shutdownVM(demux, handle)
	return nil
}

// StartInstance sets Enabled=true and boots the instance.
// This is the explicit start path (CLI/API) — distinct from EnsureInstance (auto-wake).
func (m *Manager) StartInstance(ctx context.Context, id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	inst.Enabled = true
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

// DisableInstance makes an instance a pure registry object:
// sets Enabled=false, closes exec waiters, cancels timers, tears down demuxer, stops VM.
func (m *Manager) DisableInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	// Set disabled first so router/EnsureInstance stops waking immediately
	inst.Enabled = false

	state := inst.State
	if state == StateStopped {
		// Already stopped — just ensure disabled flag is set
		inst.mu.Unlock()
		return nil
	}

	handle := inst.Handle
	demux := inst.demuxer

	// Cancel timers
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}

	// Close all exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.State = StateStopped
	inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	m.notifyStateChange(id, StateStopped)

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	if demux != nil {
		demux.Stop()
	}
	m.vmm.StopVM(handle)
	return nil
}

// DeleteInstance stops an instance and removes it entirely (from map, registry, logs).
func (m *Manager) DeleteInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	demux := inst.demuxer

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}

	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.State = StateStopped
		inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()
	m.notifyStateChange(id, StateStopped)

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	if state != StateStopped {
		m.shutdownVM(demux, handle)
	}

	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()

	m.logStore.Remove(id)

	return nil
}

// shutdownVM sends shutdown RPC and stops the VM.
func (m *Manager) shutdownVM(demux *channelDemuxer, handle vmm.Handle) {
	if demux != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		demux.Call(shutCtx, "shutdown", nil, 99)
		cancel()
		demux.Stop()
	}
	m.vmm.StopVM(handle)
}

// Shutdown deletes all instances (used on daemon exit).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.DeleteInstance(id); err != nil {
			log.Printf("delete instance %s: %v", id, err)
		}
	}
}

// ListInstances returns all active instances.
func (m *Manager) ListInstances() []*Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		result = append(result, inst)
	}
	return result
}

// ExecInstance runs a command in a running instance via the demuxer.
// Returns the exec ID, start time, a done channel that receives the exit code, and any error.
func (m *Manager) ExecInstance(ctx context.Context, id string, command []string, env map[string]string) (string, time.Time, <-chan int, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return "", time.Time{}, nil, fmt.Errorf("instance not found")
	}

	inst.mu.Lock()
	enabled := inst.Enabled
	state := inst.State
	inst.mu.Unlock()

	if !enabled {
		return "", time.Time{}, nil, ErrInstanceDisabled
	}

	switch state {
	case StateRunning:
		// proceed
	case StatePaused:
		if err := m.resumeInstance(inst); err != nil {
			return "", time.Time{}, nil, fmt.Errorf("resume for exec: %w", err)
		}
	case StateStopped:
		return "", time.Time{}, nil, ErrInstanceStopped
	case StateStarting:
		if err := m.waitForRunning(ctx, inst); err != nil {
			return "", time.Time{}, nil, err
		}
	}

	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()

	if demux == nil {
		return "", time.Time{}, nil, fmt.Errorf("instance has no active channel")
	}

	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	startedAt := time.Now()

	// Register done channel before sending RPC to avoid race
	doneCh := make(chan int, 1)
	inst.mu.Lock()
	if inst.execWaiters == nil {
		inst.execWaiters = make(map[string]chan int)
	}
	inst.execWaiters[execID] = doneCh
	inst.mu.Unlock()

	resp, err := demux.Call(ctx, "exec", map[string]interface{}{
		"command": command,
		"env":     env,
		"exec_id": execID,
	}, nextRPCID())
	if err != nil {
		inst.mu.Lock()
		delete(inst.execWaiters, execID)
		inst.mu.Unlock()
		return "", time.Time{}, nil, fmt.Errorf("exec RPC: %w", err)
	}

	// Check for error in response
	var respObj struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &respObj) == nil && respObj.Error != nil {
		inst.mu.Lock()
		delete(inst.execWaiters, execID)
		inst.mu.Unlock()
		return "", time.Time{}, nil, fmt.Errorf("exec failed: %s", respObj.Error.Message)
	}

	return execID, startedAt, doneCh, nil
}

// ActiveConns returns the active connection count for an instance.
func (m *Manager) ActiveConns(id string) int {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.activeConns
}

// LastActivity returns the last activity time for an instance.
func (m *Manager) LastActivity(id string) time.Time {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.lastActivity
}

func (m *Manager) notifyStateChange(id, state string) {
	if m.onStateChange != nil {
		m.onStateChange(id, state)
	}
}
