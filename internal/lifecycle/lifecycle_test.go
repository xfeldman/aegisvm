package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/vmm"
)

// mockVMM implements vmm.VMM for lifecycle integration tests.
// StartVM returns a mockChannel that auto-responds to the "run" RPC.
type mockVMM struct {
	mu        sync.Mutex
	created   map[string]vmm.VMConfig
	started   map[string]bool
	stopped   map[string]bool
	channels  map[string]*mockChannel
	pauseErr  error
	resumeErr error
}

func newMockVMM() *mockVMM {
	return &mockVMM{
		created:  make(map[string]vmm.VMConfig),
		started:  make(map[string]bool),
		stopped:  make(map[string]bool),
		channels: make(map[string]*mockChannel),
	}
}

func (m *mockVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("mock-vm-%d", len(m.created)+1)
	m.created[id] = cfg
	return vmm.Handle{ID: id}, nil
}

func (m *mockVMM) StartVM(h vmm.Handle) (vmm.ControlChannel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started[h.ID] = true
	ch := newMockChannel()
	m.channels[h.ID] = ch

	// Auto-respond to RPCs in background (polls for sent messages)
	go m.autoRespond(ch)

	return ch, nil
}

// autoRespond polls the mock channel's send buffer and responds to RPCs.
func (m *mockVMM) autoRespond(ch *mockChannel) {
	responded := 0
	for {
		ch.mu.Lock()
		closed := ch.closed
		pending := len(ch.sendBuf)
		ch.mu.Unlock()

		if closed {
			return
		}

		for responded < pending {
			ch.mu.Lock()
			msg := ch.sendBuf[responded]
			ch.mu.Unlock()
			responded++

			var req map[string]interface{}
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			rpcID := req["id"]
			method, _ := req["method"].(string)

			var resp []byte
			switch method {
			case "run":
				resp, _ = json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      rpcID,
					"result":  map[string]interface{}{"pid": 42, "started_at": time.Now().Format(time.RFC3339)},
				})
			case "shutdown":
				resp, _ = json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      rpcID,
					"result":  map[string]interface{}{"status": "shutting_down"},
				})
			default:
				continue
			}
			select {
			case ch.recvCh <- resp:
			case <-ch.closedCh:
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (m *mockVMM) PauseVM(h vmm.Handle) error  { return m.pauseErr }
func (m *mockVMM) ResumeVM(h vmm.Handle) error { return m.resumeErr }

func (m *mockVMM) StopVM(h vmm.Handle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped[h.ID] = true
	if ch, ok := m.channels[h.ID]; ok {
		ch.Close()
	}
	return nil
}

func (m *mockVMM) HostEndpoints(h vmm.Handle) ([]vmm.HostEndpoint, error) {
	return nil, nil
}

func (m *mockVMM) Capabilities() vmm.BackendCaps {
	return vmm.BackendCaps{
		Pause:      true,
		RootFSType: vmm.RootFSDirectory,
		Name:       "mock",
	}
}

func (m *mockVMM) stoppedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stopped)
}

func newTestManagerWithVMM(v vmm.VMM) *Manager {
	cfg := &config.Config{
		BaseRootfsPath:  "/tmp/test-rootfs",
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		PauseAfterIdle:  60 * time.Second,
		StopAfterIdle:   20 * time.Minute,
	}
	dir, _ := os.MkdirTemp("", "aegis-test-logs-*")
	ls := logstore.NewStore(dir)
	return NewManager(v, cfg, ls, nil, nil)
}

// --- Boot + Stop lifecycle tests ---

func TestEnsureInstance_BootFromStopped(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	inst := m.CreateInstance("inst-1", []string{"echo", "hello"}, nil)

	if inst.State != StateStopped {
		t.Fatalf("initial state = %q, want %q", inst.State, StateStopped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := m.EnsureInstance(ctx, "inst-1")
	if err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}

	if inst.State != StateRunning {
		t.Errorf("state after boot = %q, want %q", inst.State, StateRunning)
	}
}

func TestEnsureInstance_StoppedAtClearedOnBoot(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil)

	// Manually set StoppedAt as if previously stopped
	inst.mu.Lock()
	inst.StoppedAt = time.Now().Add(-1 * time.Hour)
	inst.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := m.EnsureInstance(ctx, "inst-1")
	if err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}

	if !inst.StoppedAt.IsZero() {
		t.Errorf("StoppedAt should be cleared on boot, got %v", inst.StoppedAt)
	}
}

func TestEnsureInstance_AlreadyRunning(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Boot it
	m.EnsureInstance(ctx, "inst-1")

	// Ensure again — should be a noop
	err := m.EnsureInstance(ctx, "inst-1")
	if err != nil {
		t.Fatalf("EnsureInstance on running: %v", err)
	}
}

func TestStopInstance_SetsStoppedAt(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Boot it
	if err := m.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	before := time.Now()
	if err := m.StopInstance("inst-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	after := time.Now()

	if inst.State != StateStopped {
		t.Errorf("state after stop = %q, want %q", inst.State, StateStopped)
	}
	if inst.StoppedAt.IsZero() {
		t.Fatal("StoppedAt should be set after stop")
	}
	if inst.StoppedAt.Before(before) || inst.StoppedAt.After(after) {
		t.Errorf("StoppedAt = %v, want between %v and %v", inst.StoppedAt, before, after)
	}
}

func TestStopInstance_PreservesConfig(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	env := map[string]string{"API_KEY": "sk-123"}
	inst := m.CreateInstance("inst-1", []string{"python", "app.py"}, []vmm.PortExpose{
		{GuestPort: 80, Protocol: "http"},
	},
		WithHandle("web"),
		WithWorkspace("/data/myapp"),
		WithEnv(env),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.EnsureInstance(ctx, "inst-1")
	m.StopInstance("inst-1")

	// Config should be preserved
	if inst.HandleAlias != "web" {
		t.Errorf("HandleAlias lost after stop: %q", inst.HandleAlias)
	}
	if inst.WorkspacePath != "/data/myapp" {
		t.Errorf("WorkspacePath lost after stop: %q", inst.WorkspacePath)
	}
	if inst.Env["API_KEY"] != "sk-123" {
		t.Errorf("Env lost after stop: %v", inst.Env)
	}
	if len(inst.Command) != 2 || inst.Command[0] != "python" {
		t.Errorf("Command lost after stop: %v", inst.Command)
	}

	// Runtime state should be cleared
	if inst.Channel != nil {
		t.Error("Channel should be nil after stop")
	}
	if inst.Endpoints != nil {
		t.Error("Endpoints should be nil after stop")
	}
}

func TestStopInstance_AlreadyStopped(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	// Stop without booting — already stopped
	err := m.StopInstance("inst-1")
	if err != nil {
		t.Fatalf("stop already-stopped: %v", err)
	}
}

func TestStopInstance_NotFound(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	err := m.StopInstance("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

// --- Restart from stopped ---

func TestRestartFromStopped(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	inst := m.CreateInstance("inst-1", []string{"python", "app.py"}, nil,
		WithHandle("web"),
		WithWorkspace("/data/myapp"),
		WithEnv(map[string]string{"KEY": "value"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Boot → stop → restart
	if err := m.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state after first boot = %q", inst.State)
	}

	if err := m.StopInstance("inst-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if inst.State != StateStopped {
		t.Fatalf("state after stop = %q", inst.State)
	}
	if inst.StoppedAt.IsZero() {
		t.Fatal("StoppedAt should be set")
	}

	// Restart
	if err := m.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state after restart = %q, want running", inst.State)
	}
	if !inst.StoppedAt.IsZero() {
		t.Error("StoppedAt should be cleared after restart")
	}

	// Config should still be intact
	if inst.HandleAlias != "web" {
		t.Errorf("HandleAlias = %q after restart", inst.HandleAlias)
	}
	if inst.WorkspacePath != "/data/myapp" {
		t.Errorf("WorkspacePath = %q after restart", inst.WorkspacePath)
	}
	if inst.Env["KEY"] != "value" {
		t.Errorf("Env = %v after restart", inst.Env)
	}
}

// --- Delete ---

func TestDeleteInstance_RemovesFromMap(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithHandle("web"))

	if err := m.DeleteInstance("inst-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if m.GetInstance("inst-1") != nil {
		t.Error("instance should be removed from map after delete")
	}
	if m.GetInstanceByHandle("web") != nil {
		t.Error("instance should not be findable by handle after delete")
	}
}

func TestDeleteInstance_RunningInstance(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.EnsureInstance(ctx, "inst-1")

	if err := m.DeleteInstance("inst-1"); err != nil {
		t.Fatalf("delete running: %v", err)
	}

	if m.GetInstance("inst-1") != nil {
		t.Error("running instance should be removed after delete")
	}
	if mv.stoppedCount() == 0 {
		t.Error("VMM StopVM should have been called")
	}
}

func TestDeleteInstance_NotFound(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	err := m.DeleteInstance("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func TestDeleteInstance_RemovesLogs(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	// Write a log entry
	il := m.logStore.GetOrCreate("inst-1")
	il.Append("stdout", "hello", "", "server")

	m.DeleteInstance("inst-1")

	// Log store should return a fresh (empty) entry after deletion
	il2 := m.logStore.GetOrCreate("inst-1")
	entries := il2.Read(time.Time{}, 0)
	if len(entries) != 0 {
		t.Errorf("expected empty logs after delete, got %d entries", len(entries))
	}
}

// --- State change callback ---

func TestStateChangeCallback_BootStopRestart(t *testing.T) {
	mv := newMockVMM()
	m := newTestManagerWithVMM(mv)

	var states []string
	m.OnStateChange(func(id, state string) {
		states = append(states, state)
	})

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.EnsureInstance(ctx, "inst-1")
	m.StopInstance("inst-1")
	m.EnsureInstance(ctx, "inst-1")

	// Expected: starting, running, stopped, starting, running
	if len(states) < 4 {
		t.Fatalf("expected at least 4 state changes, got %d: %v", len(states), states)
	}

	// First boot: starting → running
	if states[0] != StateStarting {
		t.Errorf("states[0] = %q, want starting", states[0])
	}
	if states[1] != StateRunning {
		t.Errorf("states[1] = %q, want running", states[1])
	}
	// Stop
	if states[2] != StateStopped {
		t.Errorf("states[2] = %q, want stopped", states[2])
	}
	// Restart: starting → running
	if states[3] != StateStarting {
		t.Errorf("states[3] = %q, want starting", states[3])
	}
}
