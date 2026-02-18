package lifecycle

import (
	"testing"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/vmm"
)

// newTestManager creates a Manager with a nil VMM and minimal config.
// This is safe because the tests here only exercise CreateInstance,
// GetInstance, GetInstanceByApp, and option patterns — none of which
// call into the VMM backend.
func newTestManager() *Manager {
	cfg := &config.Config{
		BaseRootfsPath:     "/tmp/test-rootfs",
		DefaultMemoryMB:    256,
		DefaultVCPUs:       1,
		PauseAfterIdle:     60 * time.Second,
		TerminateAfterIdle: 20 * time.Minute,
	}
	return NewManager(nil, cfg)
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

func TestCreateInstance_WithAppOption(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"node", "server.js"}, nil,
		WithApp("app-123", "rel-456"),
	)

	if inst.AppID != "app-123" {
		t.Errorf("AppID = %q, want %q", inst.AppID, "app-123")
	}
	if inst.ReleaseID != "rel-456" {
		t.Errorf("ReleaseID = %q, want %q", inst.ReleaseID, "rel-456")
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
		WithApp("myapp", "v2"),
		WithRootfs("/releases/v2/rootfs"),
		WithWorkspace("/workspaces/myapp"),
	)

	if inst.AppID != "myapp" {
		t.Errorf("AppID = %q, want %q", inst.AppID, "myapp")
	}
	if inst.ReleaseID != "v2" {
		t.Errorf("ReleaseID = %q, want %q", inst.ReleaseID, "v2")
	}
	if inst.RootfsPath != "/releases/v2/rootfs" {
		t.Errorf("RootfsPath = %q, want %q", inst.RootfsPath, "/releases/v2/rootfs")
	}
	if inst.WorkspacePath != "/workspaces/myapp" {
		t.Errorf("WorkspacePath = %q, want %q", inst.WorkspacePath, "/workspaces/myapp")
	}
}

func TestCreateInstance_NoOptions(t *testing.T) {
	m := newTestManager()

	inst := m.CreateInstance("inst-1", []string{"echo", "hello"}, nil)

	// Without options, app/release/rootfs/workspace should be zero values
	if inst.AppID != "" {
		t.Errorf("AppID = %q, want empty", inst.AppID)
	}
	if inst.ReleaseID != "" {
		t.Errorf("ReleaseID = %q, want empty", inst.ReleaseID)
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

func TestGetInstanceByApp_Found(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithApp("app-A", "rel-1"))
	m.CreateInstance("inst-2", []string{"echo"}, nil, WithApp("app-B", "rel-2"))

	got := m.GetInstanceByApp("app-B")
	if got == nil {
		t.Fatal("expected instance for app-B, got nil")
	}
	if got.ID != "inst-2" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-2")
	}
	if got.AppID != "app-B" {
		t.Errorf("AppID = %q, want %q", got.AppID, "app-B")
	}
}

func TestGetInstanceByApp_NotFound(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"echo"}, nil, WithApp("app-A", "rel-1"))

	got := m.GetInstanceByApp("nonexistent-app")
	if got != nil {
		t.Errorf("expected nil for nonexistent app, got %+v", got)
	}
}

func TestGetInstanceByApp_EmptyManager(t *testing.T) {
	m := newTestManager()

	got := m.GetInstanceByApp("any-app")
	if got != nil {
		t.Errorf("expected nil on empty manager, got %+v", got)
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

	// Both should be retrievable
	if m.GetInstance("inst-1") == nil {
		t.Error("inst-1 not found in manager")
	}
	if m.GetInstance("inst-2") == nil {
		t.Error("inst-2 not found in manager")
	}
}

func TestCreateInstance_OverwritesSameID(t *testing.T) {
	m := newTestManager()

	m.CreateInstance("inst-1", []string{"old-cmd"}, nil)
	m.CreateInstance("inst-1", []string{"new-cmd"}, nil)

	got := m.GetInstance("inst-1")
	if got == nil {
		t.Fatal("expected instance, got nil")
	}
	if len(got.Command) != 1 || got.Command[0] != "new-cmd" {
		t.Errorf("Command = %v, want [new-cmd] (second create should overwrite)", got.Command)
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

	// notifyStateChange is called internally; test via the exported method path.
	// We can test it indirectly by verifying the callback is stored.
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

	// Should not panic when no callback is registered
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

	// Manually set endpoints as if the VM had been started
	inst.mu.Lock()
	inst.Endpoints = []vmm.HostEndpoint{
		{GuestPort: 8080, HostPort: 49152, Protocol: "http"},
	}
	inst.mu.Unlock()

	// Ask for a port that was not exposed
	_, err := m.GetEndpoint("inst-1", 9090)
	if err == nil {
		t.Fatal("expected error for non-matching guest port")
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
