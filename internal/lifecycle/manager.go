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
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/image"
	"github.com/xfeldman/aegisvm/internal/kit"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/overlay"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/secrets"
	"github.com/xfeldman/aegisvm/internal/tether"
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
	Enabled     bool   // policy flag: true = wake-on-connect allowed, false = unreachable
	channelGen  uint64 // monotonic, incremented on each new ControlChannel (for quiesce protocol)
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

	// Kit is the name of the kit used to create this instance (empty = no kit).
	Kit string

	// Idle policy: "default" (heartbeat + lease + inbound), "leases_only" (lease + inbound only)
	IdlePolicy string

	// Parent-child relationship for guest orchestration
	ParentID     string           // ID of the parent instance that spawned this one (empty = top-level)
	Capabilities *CapabilityToken // spawn capabilities (nil = no guest API spawning)

	// Keepalive lease — prevents pause while held, auto-expires after TTL
	leaseHeld   bool
	leaseExpiry time.Time
	leaseReason string
	leaseTimer  *time.Timer

	// Demuxer for persistent channel Recv loop (nil when stopped)
	demuxer    *channelDemuxer
	logCapture bool // guard against double-attach

	// Exec completion tracking: execID → channel that receives exit code
	execWaiters map[string]chan int

	// Crash backoff — prevents rapid restart loops
	crashCount    int       // consecutive crashes (reset on successful run > 10s)
	lastCrashAt   time.Time // time of last crash

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

	imageCache  *image.Cache
	overlay     overlay.Overlay
	secretStore *secrets.Store
	registry    *registry.DB
	tetherStore *tether.Store

	// Callbacks
	onStateChange   func(id, state string)
	onAllocatePorts func(inst *Instance)                            // router port allocation
	getPublicPorts  func(id string) map[int]int                     // guestPort → publicPort lookup
	onExposePort    func(id string, guestPort, publicPort int, protocol string) (int, error) // runtime expose
	onUnexposePort  func(id string, guestPort int)                  // runtime unexpose
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

// SetSecretStore sets the secret store for capability token operations.
func (m *Manager) SetSecretStore(ss *secrets.Store) {
	m.secretStore = ss
}

// saveToRegistry persists an instance to the registry database.
func (m *Manager) saveToRegistry(inst *Instance) {
	capsJSON := ""
	if inst.Capabilities != nil {
		if b, err := json.Marshal(inst.Capabilities); err == nil {
			capsJSON = string(b)
		}
	}
	portInts := make([]int, len(inst.ExposePorts))
	for i, p := range inst.ExposePorts {
		portInts[i] = p.GuestPort
	}
	regInst := &registry.Instance{
		ID:           inst.ID,
		State:        "stopped",
		Command:      inst.Command,
		ExposePorts:  portInts,
		Handle:       inst.HandleAlias,
		ImageRef:     inst.ImageRef,
		Workspace:    inst.WorkspacePath,
		Env:          inst.Env,
		Enabled:      inst.Enabled,
		MemoryMB:     inst.MemoryMB,
		VCPUs:        inst.VCPUs,
		ParentID:     inst.ParentID,
		Capabilities: capsJSON,
		Kit:          inst.Kit,
		CreatedAt:    inst.CreatedAt,
	}
	if err := m.registry.SaveInstance(regInst); err != nil {
		log.Printf("save instance to registry: %v", err)
	}
}

// OnStateChange registers a callback for state changes (e.g., to persist to registry).
func (m *Manager) OnStateChange(fn func(id, state string)) {
	m.onStateChange = fn
}

// OnAllocatePorts registers a callback for router port allocation.
// Called from CreateInstance for every new instance (host or guest).
func (m *Manager) OnAllocatePorts(fn func(inst *Instance)) {
	m.onAllocatePorts = fn
}

// OnExposePort registers a callback for runtime port expose.
// The callback allocates a router port and returns the public port.
func (m *Manager) OnExposePort(fn func(id string, guestPort, publicPort int, protocol string) (int, error)) {
	m.onExposePort = fn
}

// OnUnexposePort registers a callback for runtime port unexpose.
func (m *Manager) OnUnexposePort(fn func(id string, guestPort int)) {
	m.onUnexposePort = fn
}

// SetPublicPortsLookup registers a function to query router public ports.
func (m *Manager) SetPublicPortsLookup(fn func(id string) map[int]int) {
	m.getPublicPorts = fn
}

// GetPublicPorts returns guestPort → publicPort mapping for an instance.
func (m *Manager) GetPublicPorts(id string) map[int]int {
	if m.getPublicPorts != nil {
		return m.getPublicPorts(id)
	}
	return nil
}

// SetRegistry sets the registry for instance persistence.
func (m *Manager) SetRegistry(reg *registry.DB) {
	m.registry = reg
}

// SetTetherStore sets the tether store for agent kit messaging.
func (m *Manager) SetTetherStore(ts *tether.Store) {
	m.tetherStore = ts
}

// TetherStore returns the tether store (for API layer access).
func (m *Manager) TetherStore() *tether.Store {
	return m.tetherStore
}

// SendTetherFrame sends an ingress tether frame to a running instance.
func (m *Manager) SendTetherFrame(instanceID string, frame tether.Frame) error {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux == nil {
		return fmt.Errorf("instance %s not connected", instanceID)
	}

	return demux.SendNotification("tether.frame", frame)
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

// WithParentID sets the parent instance ID (for child instances spawned via guest API).
func WithParentID(id string) InstanceOption {
	return func(inst *Instance) {
		inst.ParentID = id
	}
}

// WithCapabilities sets the spawn capabilities for guest orchestration.
func WithCapabilities(caps *CapabilityToken) InstanceOption {
	return func(inst *Instance) {
		inst.Capabilities = caps
	}
}

// WithIdlePolicy sets the idle detection policy.
// "default": heartbeat + lease + inbound connections prevent pause.
// "leases_only": only leases and inbound connections prevent pause (heartbeat ignored).
func WithIdlePolicy(policy string) InstanceOption {
	return func(inst *Instance) {
		if policy == "leases_only" {
			inst.IdlePolicy = policy
		}
		// "default" or empty = default behavior
	}
}

// WithEnabled sets the enabled policy flag.
func WithEnabled(enabled bool) InstanceOption {
	return func(inst *Instance) {
		inst.Enabled = enabled
	}
}

// WithKit sets the kit name for this instance.
func WithKit(name string) InstanceOption {
	return func(inst *Instance) {
		inst.Kit = name
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
	m.logStore.GetOrCreate(id)

	// Allocate router ports (external concern, callback)
	if m.onAllocatePorts != nil {
		m.onAllocatePorts(inst)
	}

	// Save to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

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

	// Crash backoff: if the instance crashed recently, delay or refuse boot.
	// Max 5 consecutive crashes within a 2-minute window. Resets after 2 minutes.
	if inst.crashCount > 0 && !inst.lastCrashAt.IsZero() {
		if time.Since(inst.lastCrashAt) > 2*time.Minute {
			// Window expired — reset
			inst.crashCount = 0
		} else if inst.crashCount >= 5 {
			inst.mu.Unlock()
			return fmt.Errorf("instance %s crash loop (crashed %d times, last at %s)",
				inst.ID, inst.crashCount, inst.lastCrashAt.Format(time.RFC3339))
		}
	}

	inst.State = StateStarting
	inst.StoppedAt = time.Time{} // clear
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateStarting)

	// If image ref is set but no rootfs yet, prepare it
	if inst.ImageRef != "" && inst.RootfsPath == "" {
		log.Printf("instance %s: preparing image rootfs for %s", inst.ID, inst.ImageRef)
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

	log.Printf("instance %s: rootfs ready, starting VM", inst.ID)

	rootfsPath := m.cfg.BaseRootfsPath
	if inst.RootfsPath != "" {
		rootfsPath = inst.RootfsPath
	}

	// If the backend needs a block image and we have a directory, convert it
	needsBlock := m.vmm.Capabilities().RootFSType == vmm.RootFSBlockImage
	if needsBlock {
		if info, err := os.Stat(rootfsPath); err == nil && info.IsDir() {
			log.Printf("instance %s: converting directory rootfs to ext4: %s", inst.ID, rootfsPath)
			ext4Path, err := dirToExt4(rootfsPath, 512)
			if err != nil {
				inst.mu.Lock()
				inst.State = StateStopped
				inst.StoppedAt = time.Now()
				inst.mu.Unlock()
				m.notifyStateChange(inst.ID, StateStopped)
				return fmt.Errorf("convert rootfs to ext4: %w", err)
			}
			rootfsPath = ext4Path
		}
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

	// If a snapshot exists for this instance, tell the VMM to restore from it.
	// On restore, the primary process is already running — skip the run RPC.
	isRestore := false
	snapshotDir := filepath.Join(m.cfg.SnapshotsDir, inst.ID)
	if info, err := os.Stat(snapshotDir); err == nil && info.IsDir() {
		if setter, ok := m.vmm.(interface{ SetSnapshotDir(vmm.Handle, string) error }); ok {
			if err := setter.SetSnapshotDir(handle, snapshotDir); err != nil {
				log.Printf("instance %s: set snapshot dir: %v (proceeding with fresh boot)", inst.ID, err)
			} else {
				log.Printf("instance %s: will restore from snapshot %s", inst.ID, snapshotDir)
				isRestore = true
			}
		}
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
		case "activity":
			m.bumpActivity(inst)
		case "keepalive":
			var kp struct {
				TTLMs  int    `json:"ttl_ms"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(params, &kp) == nil && kp.TTLMs > 0 {
				m.acquireLease(inst, kp.TTLMs, kp.Reason)
			}
		case "keepalive.release":
			m.releaseLease(inst)
		case "tether.frame":
			if m.tetherStore != nil {
				var frame tether.Frame
				if json.Unmarshal(params, &frame) == nil && frame.IsEgress() {
					m.tetherStore.Append(inst.ID, frame)
				}
			}
		}
	})

	// Register guest request handler (for guest API → aegisd calls)
	demux.onGuestRequest = func(method string, params json.RawMessage) (interface{}, error) {
		return m.handleGuestRequest(inst, method, params)
	}

	// Send run RPC — only on fresh boot, not on restore (primary process already running).
	if !isRestore {
		rpcCtx, rpcCancel := context.WithTimeout(ctx, 30*time.Second)
		defer rpcCancel()

		rpcParams := map[string]interface{}{
			"command": inst.Command,
		}
		if len(inst.Env) > 0 {
			rpcParams["env"] = inst.Env
		}
		if len(inst.ExposePorts) > 0 {
			var ports []int
			for _, ep := range inst.ExposePorts {
				ports = append(ports, ep.GuestPort)
			}
			rpcParams["expose_ports"] = ports
		}
		log.Printf("instance %s: run RPC params: expose_ports=%v", inst.ID, rpcParams["expose_ports"])

		// Inject capability token if this instance has spawn capabilities
		if inst.Capabilities != nil && m.secretStore != nil {
			token, err := GenerateToken(m.secretStore, inst.ID, *inst.Capabilities)
			if err != nil {
				log.Printf("instance %s: generate capability token: %v", inst.ID, err)
			} else {
				rpcParams["capability_token"] = token
			}
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
	} else {
		log.Printf("instance %s: restored from snapshot, skipping run RPC (primary process already running)", inst.ID)
		// Clean up snapshot after successful restore — it's a one-time use.
		os.RemoveAll(snapshotDir)
	}

	// Instance is RUNNING immediately after run RPC succeeds (no readiness wait)
	inst.mu.Lock()
	inst.Handle = handle
	inst.Channel = ch
	inst.channelGen++
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
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}
	inst.leaseHeld = false

	// Close exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	// Track crash backoff: if process ran < 10s, count as a crash
	uptime := time.Since(inst.lastActivity)
	if exitCode != 0 && uptime < 10*time.Second {
		inst.crashCount++
		inst.lastCrashAt = time.Now()
		log.Printf("instance %s: crash #%d (uptime=%v)", inst.ID, inst.crashCount, uptime)
	} else {
		inst.crashCount = 0 // clean exit or long-lived process — reset
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

	// Clean up stale snapshot (process crashed/exited, snapshot is stale)
	snapshotDir := filepath.Join(m.cfg.SnapshotsDir, inst.ID)
	if _, err := os.Stat(snapshotDir); err == nil {
		os.RemoveAll(snapshotDir)
		log.Printf("instance %s: removed stale snapshot (process exited)", inst.ID)
	}

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

// bumpActivity resets the idle timer when the harness reports guest activity.
// Called on "activity" notifications from the harness (outbound connections, CPU, network).
// Only effective when idle_policy is "default" (not "leases_only").
func (m *Manager) bumpActivity(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	// leases_only policy ignores heartbeat activity
	if inst.IdlePolicy == "leases_only" {
		return
	}

	inst.lastActivity = time.Now()
	if inst.State == StateRunning && inst.activeConns == 0 && !inst.leaseHeld {
		if inst.idleTimer != nil {
			inst.idleTimer.Stop()
		}
		inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
			m.pauseInstance(inst)
		})
	}
}

// acquireLease prevents the instance from pausing until the lease expires or is released.
func (m *Manager) acquireLease(inst *Instance, ttlMs int, reason string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	inst.leaseHeld = true
	inst.leaseReason = reason
	inst.leaseExpiry = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
	inst.lastActivity = time.Now()

	// Cancel idle timer while lease is held
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
	}

	// Set lease expiry timer
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
	}
	inst.leaseTimer = time.AfterFunc(time.Duration(ttlMs)*time.Millisecond, func() {
		m.releaseLease(inst)
	})

	log.Printf("instance %s: lease acquired (ttl=%dms, reason=%s)", inst.ID, ttlMs, reason)
}

// releaseLease clears the lease and restarts the idle timer if appropriate.
func (m *Manager) releaseLease(inst *Instance) {
	inst.mu.Lock()
	wasHeld := inst.leaseHeld
	inst.leaseHeld = false
	inst.leaseReason = ""
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}

	// Restart idle timer now that lease is gone
	if inst.State == StateRunning && inst.activeConns == 0 {
		if inst.idleTimer != nil {
			inst.idleTimer.Stop()
		}
		inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
			m.pauseInstance(inst)
		})
	}
	inst.mu.Unlock()

	if wasHeld {
		log.Printf("instance %s: lease released", inst.ID)
	}
}

func (m *Manager) pauseInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StateRunning || inst.activeConns > 0 || inst.leaseHeld {
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

	// Start terminate timer — unless backend has persistent pause (e.g. libkrun/macOS),
	// where the OS manages memory pressure via swap and stopping is unnecessary.
	if !m.vmm.Capabilities().PersistentPause {
		inst.mu.Lock()
		inst.stopTimer = time.AfterFunc(m.cfg.StopAfterIdle, func() {
			m.stopIdleInstance(inst)
		})
		inst.mu.Unlock()
	}
}

func (m *Manager) stopIdleInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StatePaused {
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	demux := inst.demuxer
	instID := inst.ID
	gen := inst.channelGen
	inst.mu.Unlock()

	// 1. Resume VM so the harness can respond to quiesce and see the EOF.
	// The VM is currently paused from the idle timer.
	if err := m.vmm.ResumeVM(handle); err != nil {
		log.Printf("instance %s: resume for quiesce failed: %v", instID, err)
	}

	// 2. Tell harness to quiesce — it will stop optional traffic and ACK.
	if demux != nil {
		qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := demux.Call(qctx, "quiesce.stop", map[string]interface{}{
			"channel_gen": gen,
		}, nextRPCID())
		qcancel()
		if err != nil {
			log.Printf("instance %s: quiesce.stop failed (proceeding): %v", instID, err)
		}
	}

	// 3. Close the channel — harness sees EOF, enters reconnect-wait loop.
	if demux != nil {
		demux.Stop()
	}

	// 4. Update instance state under lock.
	inst.mu.Lock()
	inst.State = StateStopped
	inst.StoppedAt = time.Now()
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	// 5. Pause VM — harness is in reconnect loop, vCPUs frozen.
	m.vmm.PauseVM(handle)

	// 6. Snapshot if backend supports it — captures harness in reconnect-wait state.
	if snapshotter, ok := m.vmm.(interface {
		SnapshotVM(vmm.Handle, string) error
	}); ok {
		snapshotDir := filepath.Join(m.cfg.SnapshotsDir, instID)
		if err := snapshotter.SnapshotVM(handle, snapshotDir); err != nil {
			log.Printf("instance %s: snapshot before stop failed: %v", instID, err)
			os.RemoveAll(snapshotDir)
		} else {
			log.Printf("instance %s: snapshot saved before stop", instID)
		}
	}

	// 7. Stop VM — kills CH, cleans tap/NAT.
	log.Printf("instance %s: stopped (extended idle)", instID)
	m.notifyStateChange(instID, StateStopped)
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
			host := "127.0.0.1"
			if ep.BackendAddr != "" {
				host = ep.BackendAddr
			}
			return fmt.Sprintf("%s:%d", host, ep.HostPort), nil
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

	if err := image.InjectGuestBinaries(overlayDir, m.cfg.BinDir); err != nil {
		m.overlay.Remove(overlayID)
		return "", fmt.Errorf("inject harness: %w", err)
	}

	// Inject kit binaries if this instance uses a kit
	if inst.Kit != "" {
		manifest, err := kit.LoadManifest(inst.Kit)
		if err != nil {
			m.overlay.Remove(overlayID)
			return "", fmt.Errorf("kit %q: %w", inst.Kit, err)
		}
		if err := image.InjectKitBinaries(overlayDir, m.cfg.BinDir, manifest.Image.Inject); err != nil {
			m.overlay.Remove(overlayID)
			return "", fmt.Errorf("inject kit binaries: %w", err)
		}
	}

	return overlayDir, nil
}

// dirToExt4 creates an ext4 block image from a directory.
// Used when the backend requires RootFSBlockImage but the rootfs is a directory
// (e.g. OCI image overlay or base-rootfs directory on Linux).
func dirToExt4(dir string, sizeMB int) (string, error) {
	out := dir + ".ext4"
	cmd := exec.Command("mkfs.ext4", "-d", dir, "-L", "aegis", out, fmt.Sprintf("%dM", sizeMB))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mkfs.ext4 -d %s: %w", dir, err)
	}
	return out, nil
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

	// Cascade stop children (orphan policy: cascade)
	go m.CascadeStopChildren(id)

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

	// Cascade stop children
	go m.CascadeStopChildren(id)

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

// LeaseInfo returns lease state for an instance. Returns held=false if not found.
func (m *Manager) LeaseInfo(id string) (held bool, reason string, expiresAt time.Time) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return false, "", time.Time{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.leaseHeld, inst.leaseReason, inst.leaseExpiry
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

// ExposePort adds a port mapping to a running or stopped instance.
// Returns the allocated public port.
func (m *Manager) ExposePort(id string, guestPort, publicPort int, protocol string) (int, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("instance %s not found", id)
	}

	if protocol == "" {
		protocol = "http"
	}

	// Check capability cap (max_expose_ports)
	inst.mu.Lock()
	if inst.Capabilities != nil && inst.Capabilities.MaxExposePorts > 0 {
		if len(inst.ExposePorts) >= inst.Capabilities.MaxExposePorts {
			inst.mu.Unlock()
			return 0, fmt.Errorf("max_expose_ports cap reached (%d)", inst.Capabilities.MaxExposePorts)
		}
	}
	// Check for duplicate
	for _, ep := range inst.ExposePorts {
		if ep.GuestPort == guestPort {
			inst.mu.Unlock()
			// Already exposed — return existing public port (idempotent)
			if m.getPublicPorts != nil {
				ports := m.getPublicPorts(id)
				if pp, ok := ports[guestPort]; ok {
					return pp, nil
				}
			}
			return 0, fmt.Errorf("guest port %d already exposed", guestPort)
		}
	}
	inst.ExposePorts = append(inst.ExposePorts, vmm.PortExpose{GuestPort: guestPort, Protocol: protocol})
	inst.mu.Unlock()

	// If instance is running, create VMM-level port forward (gvproxy)
	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	inst.mu.Unlock()

	if state == StateRunning {
		lkvm, ok := m.vmm.(interface {
			DynamicExposePort(vmm.Handle, int) (int, error)
		})
		if !ok {
			// Backend doesn't support runtime expose — rollback
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, fmt.Errorf("backend does not support runtime expose; restart instance to apply")
		}
		hostPort, err := lkvm.DynamicExposePort(handle, guestPort)
		if err != nil {
			// Rollback ExposePorts
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, fmt.Errorf("vmm expose: %w", err)
		}
		// Refresh lifecycle endpoints from VMM so GetEndpoint gets BackendAddr
		if eps, err := m.vmm.HostEndpoints(handle); err == nil {
			inst.mu.Lock()
			inst.Endpoints = eps
			inst.mu.Unlock()
		} else {
			// Fallback: add without BackendAddr
			inst.mu.Lock()
			inst.Endpoints = append(inst.Endpoints, vmm.HostEndpoint{
				GuestPort: guestPort,
				HostPort:  hostPort,
				Protocol:  protocol,
			})
			inst.mu.Unlock()
		}
	}

	// Allocate router port
	allocatedPort := 0
	if m.onExposePort != nil {
		var err error
		allocatedPort, err = m.onExposePort(id, guestPort, publicPort, protocol)
		if err != nil {
			// Rollback ExposePorts
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, err
		}
	}

	// Persist to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

	// Notify harness if running
	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux != nil {
		demux.SendNotification("ports_changed", map[string]interface{}{
			"action":      "expose",
			"guest_port":  guestPort,
			"public_port": allocatedPort,
			"protocol":    protocol,
		})
	}

	return allocatedPort, nil
}

// UnexposePort removes a port mapping from an instance.
func (m *Manager) UnexposePort(id string, guestPort int) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	found := false
	for i, ep := range inst.ExposePorts {
		if ep.GuestPort == guestPort {
			inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
			found = true
			break
		}
	}
	inst.mu.Unlock()

	if !found {
		return fmt.Errorf("guest port %d not exposed on instance %s", guestPort, id)
	}

	// Remove VMM-level port forward and lifecycle endpoint if running
	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	// Remove from lifecycle endpoints
	for i, ep := range inst.Endpoints {
		if ep.GuestPort == guestPort {
			inst.Endpoints = append(inst.Endpoints[:i], inst.Endpoints[i+1:]...)
			break
		}
	}
	inst.mu.Unlock()

	if state == StateRunning {
		if lkvm, ok := m.vmm.(interface {
			DynamicUnexposePort(vmm.Handle, int) error
		}); ok {
			lkvm.DynamicUnexposePort(handle, guestPort)
		}
	}

	// Free router port
	if m.onUnexposePort != nil {
		m.onUnexposePort(id, guestPort)
	}

	// Persist to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

	// Notify harness if running
	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux != nil {
		demux.SendNotification("ports_changed", map[string]interface{}{
			"action":     "unexpose",
			"guest_port": guestPort,
		})
	}

	return nil
}

func (m *Manager) notifyStateChange(id, state string) {
	if m.onStateChange != nil {
		m.onStateChange(id, state)
	}
}
