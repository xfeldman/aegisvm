//go:build integration

package integration

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Linux-specific integration tests for the Cloud Hypervisor backend.
// These tests exercise stop/cold-restart and tap networking.
// They are skipped on non-Linux platforms.

func skipIfNotLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test (Cloud Hypervisor backend)")
	}
}

// TestLinux_RunEcho — basic smoke test: boot VM via CH, run command, verify output.
func TestLinux_RunEcho(t *testing.T) {
	skipIfNotLinux(t)

	out := aegisRun(t, "run", "--", "echo", "linux-ch-echo")
	if !strings.Contains(out, "linux-ch-echo") {
		t.Fatalf("expected 'linux-ch-echo' in output, got: %s", out)
	}
}

// TestLinux_PauseResume — explicit pause/resume via CH API (vm.pause / vm.resume).
func TestLinux_PauseResume(t *testing.T) {
	skipIfNotLinux(t)
	if testing.Short() {
		t.Skip("pause/resume test skipped in short mode")
	}

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "linux-pr",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Pause
	apiPost(t, fmt.Sprintf("/v1/instances/%s/pause", id), nil)
	waitForState(t, id, "paused", 10*time.Second)

	// Resume
	apiPost(t, fmt.Sprintf("/v1/instances/%s/resume", id), nil)
	waitForState(t, id, "running", 10*time.Second)

	// Verify VM is still functional after resume
	out := aegisExec(t, id, "echo", "after-resume")
	if !strings.Contains(out, "after-resume") {
		t.Fatalf("exec after resume failed, got: %s", out)
	}
}

// TestLinux_StopStart — stop a running instance, then start it again (cold boot).
func TestLinux_StopStart(t *testing.T) {
	skipIfNotLinux(t)
	if testing.Short() {
		t.Skip("stop/start test skipped in short mode")
	}

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo boot-marker-$$; sleep 300"},
		"handle":  "linux-ss",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	out := aegisExec(t, id, "echo", "before-stop")
	if !strings.Contains(out, "before-stop") {
		t.Fatalf("exec before stop failed, got: %s", out)
	}

	// Disable (stops the VM)
	apiPost(t, fmt.Sprintf("/v1/instances/%s/disable", id), nil)
	waitForState(t, id, "stopped", 30*time.Second)

	// Start again — cold boot
	apiPost(t, fmt.Sprintf("/v1/instances/%s/start", id), nil)
	waitForState(t, id, "running", 60*time.Second)

	out = aegisExec(t, id, "echo", "after-restart")
	if !strings.Contains(out, "after-restart") {
		t.Fatalf("exec after restart failed, got: %s", out)
	}
}

// TestLinux_TapNetworking — verify guest has network connectivity via tap.
func TestLinux_TapNetworking(t *testing.T) {
	skipIfNotLinux(t)

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "linux-tap-net",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Check that eth0 is configured with an IP
	out := aegisExec(t, id, "ip", "addr", "show", "eth0")
	if !strings.Contains(out, "172.16.") {
		t.Fatalf("expected 172.16.x.x IP on eth0, got: %s", out)
	}

	// Check that the default route is set
	out = aegisExec(t, id, "ip", "route", "show", "default")
	if !strings.Contains(out, "172.16.") {
		t.Fatalf("expected default route via 172.16.x.x, got: %s", out)
	}
}

// TestLinux_DirectIngress — verify that the router can reach guest ports
// directly over tap (no proxy layer).
func TestLinux_DirectIngress(t *testing.T) {
	skipIfNotLinux(t)

	const publicPort = 8282

	// Cleanup from any previous failed run
	apiDeleteAllowFail(t, "/v1/instances/linux-ingress")
	time.Sleep(500 * time.Millisecond)

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-m", "http.server", "80"},
		"handle":  "linux-ingress",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	// Expose with deterministic port
	apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", id), map[string]interface{}{
		"port":        80,
		"public_port": publicPort,
	})

	// Wait for HTTP to be reachable
	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	body, err := waitForHTTP(url, 60*time.Second)
	if err != nil {
		t.Fatalf("HTTP via tap ingress failed: %v", err)
	}
	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing, got: %.200s", body)
	}
	t.Log("direct tap ingress working")
}

// aegisExec is defined in guest_api_test.go — reused here.
