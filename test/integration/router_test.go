//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestRouterAlwaysProxy is an end-to-end acceptance test for the always-proxy
// ingress architecture. It verifies:
//   - Deterministic port mapping (--expose 8181:80)
//   - HTTP serving through the public port
//   - Wake-on-connect from PAUSED state
//   - Wake-on-connect from STOPPED state (full VM restart)
//   - Port freed after instance delete
func TestRouterAlwaysProxy(t *testing.T) {
	const (
		publicPort = 8181
		guestPort  = 80
		handle     = "router-test"
	)

	// Cleanup from any previous failed run
	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(1 * time.Second)

	// --- Step 1: Start instance, then expose with deterministic port ---
	t.Log("Step 1: start instance, then expose 8181:80")
	result := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-m", "http.server", "80"},
		"handle":  handle,
	})

	instID, _ := result["id"].(string)
	if instID == "" {
		t.Fatal("no instance ID returned")
	}
	t.Cleanup(func() {
		apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", instID))
	})

	// Expose port at runtime with deterministic public port
	exposeResult := apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", instID), map[string]interface{}{
		"port":        guestPort,
		"public_port": publicPort,
	})
	gotPublic := int(exposeResult["public_port"].(float64))
	if gotPublic != publicPort {
		t.Fatalf("expected public_port=%d, got %d", publicPort, gotPublic)
	}

	// --- Step 2: Wait for VM to boot and verify HTTP ---
	t.Log("Step 2: verify HTTP through public port")
	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	body, err := waitForHTTP(url, 30*time.Second)
	if err != nil {
		t.Fatalf("HTTP failed after boot: %v", err)
	}
	if body == "" {
		t.Fatal("empty HTTP response")
	}
	t.Logf("  HTTP 200 OK (%d bytes)", len(body))

	// --- Step 3: Pause and test wake-on-connect ---
	t.Log("Step 3: pause instance, then curl (wake-on-connect)")
	apiPost(t, fmt.Sprintf("/v1/instances/%s/pause", instID), nil)

	// Verify paused
	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	if info["state"] != "paused" {
		t.Fatalf("expected state=paused, got %s", info["state"])
	}

	// Curl the public port — should wake and respond
	start := time.Now()
	resp, err := http.Get(url)
	resumeLatency := time.Since(start)
	if err != nil {
		t.Fatalf("HTTP failed after pause: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected HTTP 200 after resume, got %d", resp.StatusCode)
	}
	t.Logf("  wake-on-connect from PAUSED: HTTP 200 in %s", resumeLatency)

	// Verify running again
	info = apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	if info["state"] != "running" {
		t.Fatalf("expected state=running after resume, got %s", info["state"])
	}

	// --- Step 4: Stop and test wake-on-connect (cold boot) ---
	t.Log("Step 4: stop instance, then curl (cold boot wake-on-connect)")
	apiPost(t, fmt.Sprintf("/v1/instances/%s/stop", instID), nil)
	time.Sleep(1 * time.Second)

	// Verify stopped
	info = apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	if info["state"] != "stopped" {
		t.Fatalf("expected state=stopped, got %s", info["state"])
	}

	// Curl the public port — should boot and respond
	start = time.Now()
	body, err = waitForHTTP(url, 30*time.Second)
	bootLatency := time.Since(start)
	if err != nil {
		t.Fatalf("HTTP failed after stop: %v", err)
	}
	if body == "" {
		t.Fatal("empty HTTP response after boot")
	}
	t.Logf("  wake-on-connect from STOPPED: HTTP 200 in %s", bootLatency)

	// Verify running again
	info = apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	if info["state"] != "running" {
		t.Fatalf("expected state=running after boot, got %s", info["state"])
	}

	// --- Step 5: Delete and verify port freed ---
	t.Log("Step 5: delete instance, verify port freed")
	apiDelete(t, fmt.Sprintf("/v1/instances/%s", instID))
	time.Sleep(500 * time.Millisecond)

	// Connection should be refused
	client := &http.Client{Timeout: 2 * time.Second}
	_, err = client.Get(url)
	if err == nil {
		t.Fatal("expected connection refused after delete, but got a response")
	}
	t.Log("  port freed: connection refused")

	t.Log("PASS: all 5 steps passed")
}
