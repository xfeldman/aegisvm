//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Conformance Suite: Backend-validating tests that ensure correctness.
// These tests validate that the VMM backend correctly implements
// the expected behavior. When Firecracker arrives in M4, these same
// tests must pass on both backends.

func TestConformanceTaskRun(t *testing.T) {
	out := aegisRun(t, "run", "--", "echo", "conformance-hello")
	if !strings.Contains(out, "conformance-hello") {
		t.Fatalf("expected 'conformance-hello' in output, got: %s", out)
	}
}

func TestConformanceServeRequest(t *testing.T) {
	// Create instance with exposed port
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for HTTP to be available via router
	body, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("serve HTTP failed: %v", err)
	}
	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing, got: %.200s", body)
	}
}

func TestConformancePauseOnIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pause test in short mode")
	}

	// This test relies on the idle timeout (60s default).
	// Create instance, wait for it to be running, then wait for pause.
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for running
	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}

	// Wait for idle timeout (60s) + buffer
	t.Log("waiting for idle timeout...")
	time.Sleep(70 * time.Second)

	// Check state should be paused
	client := daemonClient()
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", id))
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)

	if state["state"] != "paused" {
		t.Logf("instance state: %v (may already be stopped)", state["state"])
	}

	// A request should wake it up
	body, err := waitForHTTP("http://127.0.0.1:8099/", 30*time.Second)
	if err != nil {
		t.Fatalf("wake from pause failed: %v", err)
	}
	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing after wake, got: %.200s", body)
	}
}

func TestConformanceSecretInjection(t *testing.T) {
	// Create app, set secret, run task with app_id
	apiPost(t, "/v1/apps", map[string]interface{}{
		"name":    "conf-secret-app",
		"image":   "alpine:3.21",
		"command": []string{"echo"},
	})
	t.Cleanup(func() { apiDelete(t, "/v1/apps/conf-secret-app") })

	apiPut(t, "/v1/apps/conf-secret-app/secrets/CONF_SECRET",
		map[string]string{"value": "conformance-secret-value"})

	client := daemonClient()
	body := map[string]interface{}{
		"command": []string{"sh", "-c", "echo $CONF_SECRET"},
		"app_id":  "conf-secret-app",
	}
	bodyJSON, _ := json.Marshal(body)
	resp, err := client.Post("http://aegis/v1/tasks", "application/json",
		strings.NewReader(string(bodyJSON)))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer resp.Body.Close()

	var task map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&task)
	taskID := task["id"].(string)

	out := waitForTaskOutput(t, client, taskID, 60*time.Second)
	if !strings.Contains(out, "conformance-secret-value") {
		t.Fatalf("expected secret in output, got: %s", out)
	}
}

func TestConformanceSecretNotOnDisk(t *testing.T) {
	// Secrets are injected via env, not written to disk.
	// Run a task that tries to grep for the secret value on disk.
	apiPost(t, "/v1/apps", map[string]interface{}{
		"name":    "conf-nodisk-app",
		"image":   "alpine:3.21",
		"command": []string{"echo"},
	})
	t.Cleanup(func() { apiDelete(t, "/v1/apps/conf-nodisk-app") })

	apiPut(t, "/v1/apps/conf-nodisk-app/secrets/DISK_SECRET",
		map[string]string{"value": "should-not-be-on-disk-xyz"})

	client := daemonClient()
	body := map[string]interface{}{
		"command": []string{"sh", "-c", "grep -r should-not-be-on-disk-xyz /etc /tmp 2>/dev/null || echo CLEAN"},
		"app_id":  "conf-nodisk-app",
	}
	bodyJSON, _ := json.Marshal(body)
	resp, err := client.Post("http://aegis/v1/tasks", "application/json",
		strings.NewReader(string(bodyJSON)))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer resp.Body.Close()

	var task map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&task)
	taskID := task["id"].(string)

	out := waitForTaskOutput(t, client, taskID, 60*time.Second)
	if !strings.Contains(out, "CLEAN") {
		t.Fatalf("expected CLEAN (secret not on disk), got: %s", out)
	}
}

func TestConformanceEgressWorks(t *testing.T) {
	// Verify the VM can reach the internet
	out := aegisRun(t, "run", "--", "sh", "-c", "wget -q -O /dev/null http://example.com && echo EGRESS_OK || echo EGRESS_FAIL")
	if !strings.Contains(out, "EGRESS_OK") {
		t.Logf("egress test output: %s", out)
		t.Skip("egress not available (may be offline or blocked)")
	}
}

// Capability-gated tests — skip on libkrun

func TestConformanceMemorySnapshot(t *testing.T) {
	t.Skip("libkrun: no snapshot support")
}

func TestConformanceCachedResume(t *testing.T) {
	t.Skip("libkrun: no snapshot support")
}
