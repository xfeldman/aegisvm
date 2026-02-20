package lifecycle

import (
	"os"
	"testing"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/logstore"
	"github.com/xfeldman/aegis/internal/vmm"
)

// newTestManager creates a Manager with a nil VMM and minimal config.
// This is safe because the tests here only exercise CreateInstance,
// GetInstance, GetInstanceByHandle, and option patterns — none of which
// call into the VMM backend.
func newTestManager() *Manager {
	cfg := &config.Config{
		BaseRootfsPath:  "/tmp/test-rootfs",
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		PauseAfterIdle:  60 * time.Second,
		StopAfterIdle:   20 * time.Minute,
	}
	dir, _ := os.MkdirTemp("", "aegis-test-logs-*")
	ls := logstore.NewStore(dir)
	return NewManager(nil, cfg, ls, nil, nil)
}

func TestCreateInstance_Basic(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"python", "-m", "http.server"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
	})

	if inst.ID != "inst-1" {
		t.Errorf("ID = %q, want %q", inst.ID, "inst-1")
	}
	if inst.State != StateStopped {
		t.Errorf("State = %q, want %q", inst.State, StateStopped)
	}
	if len(inst.Command) != 3 || inst.Command[0] != "python" {
		t.Errorf("Command = %v, want [python -m http.server]", inst.Command)
	}
	if len(inst.ExposePorts) != 1 || inst.ExposePorts[0].GuestPort != 8080 {
		t.Errorf("ExposePorts = %v, want [{8080 http}]", inst.ExposePorts)
	}
}

func TestCreateInstance_WithHandleOption(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"node", "server.js"}, nil,
		WithHandle("myapp"),
	)

	if inst.HandleAlias != "myapp" {
		t.Errorf("HandleAlias = %q, want %q", inst.HandleAlias, "myapp")
	}
}

func TestCreateInstance_WithImageRefOption(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil,
		WithImageRef("python:3.12-alpine"),
	)

	if inst.ImageRef != "python:3.12-alpine" {
		t.Errorf("ImageRef = %q, want %q", inst.ImageRef, "python:3.12-alpine")
	}
}

func TestCreateInstance_WithRootfsOption(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil,
		WithRootfs("/custom/rootfs/path"),
	)

	if inst.RootfsPath != "/custom/rootfs/path" {
		t.Errorf("RootfsPath = %q, want %q", inst.RootfsPath, "/custom/rootfs/path")
	}
}

func TestCreateInstance_WithWorkspaceOption(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil,
		WithWorkspace("/data/workspaces/app-123"),
	)

	if inst.WorkspacePath != "/data/workspaces/app-123" {
		t.Errorf("WorkspacePath = %q, want %q", inst.WorkspacePath, "/data/workspaces/app-123")
	}
}

func TestCreateInstance_MultipleOptions(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"serve"}, []vmm.PortExpose{
		{GuestPort: 3000, Protocol: "http"},
	},
		WithHandle("myapp"),
		WithImageRef("node:18-alpine"),
		WithRootfs("/overlays/inst-1/rootfs"),
		WithWorkspace("/workspaces/myapp"),
	)

	if inst.HandleAlias != "myapp" {
		t.Errorf("HandleAlias = %q, want %q", inst.HandleAlias, "myapp")
	}
	if inst.ImageRef != "node:18-alpine" {
		t.Errorf("ImageRef = %q, want %q", inst.ImageRef, "node:18-alpine")
	}
	if inst.RootfsPath != "/overlays/inst-1/rootfs" {
		t.Errorf("RootfsPath = %q, want %q", inst.RootfsPath, "/overlays/inst-1/rootfs")
	}
	if inst.WorkspacePath != "/workspaces/myapp" {
		t.Errorf("WorkspacePath = %q, want %q", inst.WorkspacePath, "/workspaces/myapp")
	}
}

func TestCreateInstance_NoOptions(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo", "hello"}, nil)

	if inst.HandleAlias != "" {
		t.Errorf("HandleAlias = %q, want empty", inst.HandleAlias)
	}
	if inst.ImageRef != "" {
		t.Errorf("ImageRef = %q, want empty", inst.ImageRef)
	}
	if inst.RootfsPath != "" {
		t.Errorf("RootfsPath = %q, want empty", inst.RootfsPath)
	}
	if inst.WorkspacePath != "" {
		t.Errorf("WorkspacePath = %q, want empty", inst.WorkspacePath)
	}
}

func TestGetInstance(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	got := m.GetInstance("inst-1")
	if got == nil {
		t.Fatal("expected instance, got nil")
	}
	if got.ID != "inst-1" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-1")
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	m := newTestManager()

	got := m.GetInstance("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent instance, got %+v", got)
	}
}

func TestGetInstanceByHandle_Found(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithHandle("alpha"))
	m.CreateInstance("inst-2", []string{"echo"}, nil, WithHandle("beta"))

	got := m.GetInstanceByHandle("beta")
	if got == nil {
		t.Fatal("expected instance for handle beta, got nil")
	}
	if got.ID != "inst-2" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-2")
	}
}

func TestGetInstanceByHandle_NotFound(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithHandle("alpha"))

	got := m.GetInstanceByHandle("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent handle, got %+v", got)
	}
}

func TestGetDefaultInstance(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil)

	got := m.GetDefaultInstance()
	if got == nil {
		t.Fatal("expected default instance, got nil")
	}
	if got.ID != "inst-1" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-1")
	}
}

func TestGetDefaultInstance_Empty(t *testing.T) {
	m := newTestManager()

	got := m.GetDefaultInstance()
	if got != nil {
		t.Errorf("expected nil on empty manager, got %+v", got)
	}
}

func TestCreateInstance_RegisteredInMap(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil)
	m.CreateInstance("inst-2", []string{"echo"}, nil)

	if m.GetInstance("inst-1") == nil {
		t.Error("inst-1 not found in manager")
	}
	if m.GetInstance("inst-2") == nil {
		t.Error("inst-2 not found in manager")
	}
}

func TestFirstGuestPort(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
		{GuestPort: 443, Protocol: "https"},
	})

	if port := inst.FirstGuestPort(); port != 8080 {
		t.Errorf("FirstGuestPort() = %d, want 8080", port)
	}
}

func TestFirstGuestPort_NoPorts(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil)

	if port := inst.FirstGuestPort(); port != 0 {
		t.Errorf("FirstGuestPort() = %d, want 0", port)
	}
}

func TestOnStateChange_Callback(t *testing.T) {
	m := newTestManager()

	var calledID, calledState string
	m.OnStateChange(func(id, state string) {
		calledID = id
		calledState = state
	})

	m.notifyStateChange("test-id", "running")

	if calledID != "test-id" {
		t.Errorf("callback id = %q, want %q", calledID, "test-id")
	}
	if calledState != "running" {
		t.Errorf("callback state = %q, want %q", calledState, "running")
	}
}

func TestOnStateChange_NilCallback(t *testing.T) {
	m := newTestManager()
	m.notifyStateChange("test-id", "running")
}

func TestGetEndpoint_NotFound(t *testing.T) {
	m := newTestManager()

	_, err := m.GetEndpoint("nonexistent", 8080)
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func TestGetEndpoint_NoMatchingPort(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
	})

	inst.mu.Lock()
	inst.Endpoints = []vmm.HostEndpoint{
		{GuestPort: 8080, HostPort: 49152, Protocol: "http"},
	}
	inst.mu.Unlock()

	_, err := m.GetEndpoint("inst-1", 9090)
	if err == nil {
		t.Fatal("expected error for non-matching guest port")
	}
}

func TestCreateInstance_WithEnvOption(t *testing.T) {
	m := newTestManager()

	env := map[string]string{"API_KEY": "sk-123", "DEBUG": "1"}
	inst := m.CreateInstance("inst-1", []string{"echo"}, nil,
		WithEnv(env),
	)

	if inst.Env["API_KEY"] != "sk-123" {
		t.Errorf("Env[API_KEY] = %q, want %q", inst.Env["API_KEY"], "sk-123")
	}
	if inst.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q, want %q", inst.Env["DEBUG"], "1")
	}
}

func TestCreateInstance_StoppedAtInitiallyZero(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, nil)

	if !inst.StoppedAt.IsZero() {
		t.Errorf("StoppedAt should be zero on creation, got %v", inst.StoppedAt)
	}
}

func TestListInstances(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithHandle("alpha"))
	m.CreateInstance("inst-2", []string{"echo"}, nil, WithHandle("beta"))
	m.CreateInstance("inst-3", []string{"echo"}, nil)

	list := m.ListInstances()
	if len(list) != 3 {
		t.Fatalf("expected 3 instances, got %d", len(list))
	}
}

func TestListInstances_Empty(t *testing.T) {
	m := newTestManager()

	list := m.ListInstances()
	if len(list) != 0 {
		t.Fatalf("expected 0 instances, got %d", len(list))
	}
}

func TestGetEndpoint_MatchingPort(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
	})

	inst.mu.Lock()
	inst.Endpoints = []vmm.HostEndpoint{
		{GuestPort: 8080, HostPort: 49152, Protocol: "http"},
	}
	inst.mu.Unlock()

	endpoint, err := m.GetEndpoint("inst-1", 8080)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if endpoint != "127.0.0.1:49152" {
		t.Errorf("endpoint = %q, want %q", endpoint, "127.0.0.1:49152")
	}
}
