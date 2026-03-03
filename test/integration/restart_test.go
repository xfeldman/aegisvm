//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRestart_RunningInstance verifies restarting a running instance:
// create → running → restart → running again.
func TestRestart_RunningInstance(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "sleep 300"},
		"handle":  "restart-running",
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Restart via CLI
	out := aegisRun(t, "instance", "restart", "restart-running")
	if !strings.Contains(out, "restarting") {
		t.Fatalf("expected 'restarting' in output, got: %s", out)
	}

	// Should come back to running
	waitForState(t, id, "running", 60*time.Second)

	// Verify instance info is consistent after restart
	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	if info["state"] != "running" {
		t.Fatalf("expected running after restart, got %s", info["state"])
	}
	if info["enabled"] != true {
		t.Fatalf("expected enabled=true after restart")
	}
}

// TestRestart_StoppedInstance verifies restarting a stopped/disabled instance
// works idempotently (no error from the disable step).
func TestRestart_StoppedInstance(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-m", "http.server", "80"},
		"handle":  "restart-stopped",
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Disable first
	apiPost(t, fmt.Sprintf("/v1/instances/%s/disable", id), nil)
	waitForState(t, id, "stopped", 30*time.Second)

	// Restart from stopped state — should not error
	out := aegisRun(t, "instance", "restart", "restart-stopped")
	if !strings.Contains(out, "restarting") {
		t.Fatalf("expected 'restarting' in output, got: %s", out)
	}

	waitForState(t, id, "running", 60*time.Second)
}

// TestRestart_API verifies restart via the API (disable + start),
// matching the pattern used by the UI and MCP.
func TestRestart_API(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo alive && sleep 300"},
		"handle":  "restart-api",
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Disable
	apiPost(t, fmt.Sprintf("/v1/instances/%s/disable", id), nil)
	waitForState(t, id, "stopped", 30*time.Second)

	// Start (re-enable)
	apiPost(t, fmt.Sprintf("/v1/instances/%s/start", id), nil)
	waitForState(t, id, "running", 60*time.Second)

	// Verify instance info is consistent
	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	if info["state"] != "running" {
		t.Fatalf("expected running, got %s", info["state"])
	}
	if info["enabled"] != true {
		t.Fatalf("expected enabled=true after restart")
	}
}

// TestRestart_PausedInstance verifies restarting a paused instance.
func TestRestart_PausedInstance(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "sleep 300"},
		"handle":  "restart-paused",
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Pause
	apiPost(t, fmt.Sprintf("/v1/instances/%s/pause", id), nil)
	waitForState(t, id, "paused", 10*time.Second)

	// Restart from paused
	out := aegisRun(t, "instance", "restart", "restart-paused")
	if !strings.Contains(out, "restarting") {
		t.Fatalf("expected 'restarting' in output, got: %s", out)
	}

	waitForState(t, id, "running", 60*time.Second)
}
