//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestGuestAPI_SelfInfo tests that any instance can call GET /v1/self via the guest API.
func TestGuestAPI_SelfInfo(t *testing.T) {
	// Create instance (no capabilities needed for self_info)
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "guest-self-test",
		"command": []string{"sleep", "120"},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	// Wait for running
	waitForState(t, id, "running", 30*time.Second)

	// Call guest API self_info from inside the VM
	out := aegisExec(t, id, "wget", "-qO-", "http://127.0.0.1:7777/v1/self")
	if !strings.Contains(out, id) {
		t.Fatalf("self_info should contain instance ID %s, got: %s", id, out)
	}
	if !strings.Contains(out, "guest-self-test") {
		t.Fatalf("self_info should contain handle, got: %s", out)
	}
}

// TestGuestAPI_SpawnChild tests spawning a child instance from inside a VM.
func TestGuestAPI_SpawnChild(t *testing.T) {
	// Create parent with spawn capabilities
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "guest-spawn-parent",
		"command": []string{"sleep", "120"},
		"capabilities": map[string]interface{}{
			"spawn":           true,
			"spawn_depth":     2,
			"max_children":    3,
			"allowed_images":  []string{"*"},
			"max_memory_mb":   1024,
			"max_vcpus":       2,
			"max_expose_ports": 2,
		},
	})
	parentID := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+parentID)

	waitForState(t, parentID, "running", 30*time.Second)

	// Spawn a child from inside the parent VM
	out := aegisExec(t, parentID,
		"wget", "-qO-", "--post-data",
		`{"command":["echo","hello from child"],"handle":"guest-child-1"}`,
		"--header", "Content-Type: application/json",
		"http://127.0.0.1:7777/v1/instances",
	)
	if !strings.Contains(out, "guest-child-1") {
		t.Fatalf("spawn should return child handle, got: %s", out)
	}
	if !strings.Contains(out, parentID) {
		t.Fatalf("spawn should return parent_id, got: %s", out)
	}

	// List children from inside parent
	out = aegisExec(t, parentID, "wget", "-qO-", "http://127.0.0.1:7777/v1/instances")
	if !strings.Contains(out, "guest-child-1") {
		t.Fatalf("list_children should include child, got: %s", out)
	}
}

// TestGuestAPI_SpawnWithoutCapabilities tests that spawn fails without capabilities.
func TestGuestAPI_SpawnWithoutCapabilities(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "guest-no-caps",
		"command": []string{"sleep", "120"},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	waitForState(t, id, "running", 30*time.Second)

	// Try to spawn — should fail (500)
	_, err := aegisExecRaw(id, "wget", "-qO-", "--post-data",
		`{"command":["echo","nope"]}`,
		"--header", "Content-Type: application/json",
		"http://127.0.0.1:7777/v1/instances",
	)
	if err == nil {
		t.Fatal("spawn without capabilities should fail")
	}
}

// TestGuestAPI_CascadeStop tests that stopping a parent stops its children.
func TestGuestAPI_CascadeStop(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "cascade-parent",
		"command": []string{"sleep", "120"},
		"capabilities": map[string]interface{}{
			"spawn":       true,
			"spawn_depth": 1,
			"max_children": 5,
			"allowed_images": []string{"*"},
			"max_memory_mb": 1024,
		},
	})
	parentID := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+parentID)

	waitForState(t, parentID, "running", 30*time.Second)

	// Spawn a long-running child
	out := aegisExec(t, parentID,
		"wget", "-qO-", "--post-data",
		`{"command":["sleep","300"],"handle":"cascade-child"}`,
		"--header", "Content-Type: application/json",
		"http://127.0.0.1:7777/v1/instances",
	)
	if !strings.Contains(out, "cascade-child") {
		t.Fatalf("spawn failed: %s", out)
	}

	// Wait for child to be running
	time.Sleep(10 * time.Second)

	// Disable parent (stops it)
	aegisRun(t, "instance", "disable", parentID)
	time.Sleep(5 * time.Second)

	// Child should be stopped
	childInfo := apiGet(t, "/v1/instances/cascade-child")
	state, _ := childInfo["state"].(string)
	if state != "stopped" {
		t.Fatalf("child should be stopped after parent disable, got state=%s", state)
	}
}

// Helper: exec in instance, return stdout
func aegisExec(t *testing.T, id string, command ...string) string {
	t.Helper()
	args := append([]string{"exec", id, "--"}, command...)
	out, err := aegis(args...)
	if err != nil {
		t.Fatalf("aegis exec %s %v failed: %v\noutput: %s", id, command, err, out)
	}
	return out
}

// Helper: exec in instance, return stdout+error (for testing failures)
func aegisExecRaw(id string, command ...string) (string, error) {
	args := append([]string{"exec", id, "--"}, command...)
	return aegis(args...)
}

// apiGet is defined in helpers_test.go
