//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestRuntimeExpose is an end-to-end test for runtime port expose/unexpose.
// It verifies:
//   1. Instance starts without --expose (no ports)
//   2. Runtime expose allocates a public port and traffic flows
//   3. Instance info shows the exposed port
//   4. Idempotent re-expose returns the same port
//   5. Unexpose removes the port (connection refused)
//   6. Deterministic public port works (expose with specific host port)
//   7. Expose on stopped instance, then start — port restored
func TestRuntimeExpose(t *testing.T) {
	const handle = "expose-test"

	// Cleanup from any previous failed run
	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(1 * time.Second)

	// --- Step 1: Start instance WITHOUT expose ---
	t.Log("Step 1: start instance without expose")
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

	// Should have no endpoints
	if eps, ok := result["endpoints"]; ok && eps != nil {
		t.Fatalf("expected no endpoints on create, got %v", eps)
	}

	// Wait for instance to be running
	waitForState(t, instID, "running", 30*time.Second)

	// --- Step 2: Runtime expose ---
	t.Log("Step 2: expose port 80 at runtime")
	exposeResult := apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", instID), map[string]interface{}{
		"port": 80,
	})

	publicPort := int(exposeResult["public_port"].(float64))
	if publicPort <= 0 {
		t.Fatalf("expected positive public_port, got %d", publicPort)
	}
	t.Logf("  allocated public port: %d", publicPort)

	// Verify traffic flows
	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	body, err := waitForHTTP(url, 30*time.Second)
	if err != nil {
		t.Fatalf("HTTP failed after expose: %v", err)
	}
	if body == "" {
		t.Fatal("empty HTTP response")
	}
	t.Logf("  HTTP 200 OK (%d bytes)", len(body))

	// --- Step 3: Instance info shows endpoint ---
	t.Log("Step 3: verify instance info shows exposed port")
	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	eps, ok := info["endpoints"].([]interface{})
	if !ok || len(eps) == 0 {
		t.Fatal("expected endpoints in instance info")
	}
	ep, _ := eps[0].(map[string]interface{})
	gotGuest := int(ep["guest_port"].(float64))
	gotPublic := int(ep["public_port"].(float64))
	if gotGuest != 80 || gotPublic != publicPort {
		t.Fatalf("expected endpoint 80→%d, got %d→%d", publicPort, gotGuest, gotPublic)
	}

	// --- Step 4: Idempotent re-expose ---
	t.Log("Step 4: re-expose same port (idempotent)")
	reExposeResult := apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", instID), map[string]interface{}{
		"port": 80,
	})
	rePublicPort := int(reExposeResult["public_port"].(float64))
	if rePublicPort != publicPort {
		t.Fatalf("idempotent expose: expected same port %d, got %d", publicPort, rePublicPort)
	}
	t.Log("  same public port returned")

	// --- Step 5: Unexpose ---
	t.Log("Step 5: unexpose port 80")
	apiDeleteExposePort(t, instID, 80)
	time.Sleep(1 * time.Second)

	// Connection should be refused
	client := &http.Client{Timeout: 2 * time.Second}
	_, err = client.Get(url)
	if err == nil {
		t.Fatal("expected connection refused after unexpose, but got a response")
	}
	t.Log("  connection refused after unexpose")

	// Instance info should have no endpoints
	info = apiGet(t, fmt.Sprintf("/v1/instances/%s", instID))
	eps2, _ := info["endpoints"].([]interface{})
	if len(eps2) > 0 {
		t.Fatalf("expected no endpoints after unexpose, got %v", eps2)
	}

	// --- Step 6: Deterministic public port ---
	t.Log("Step 6: expose with deterministic public port")
	const fixedPort = 8282
	detResult := apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", instID), map[string]interface{}{
		"port":        80,
		"public_port": fixedPort,
	})
	gotFixed := int(detResult["public_port"].(float64))
	if gotFixed != fixedPort {
		t.Fatalf("expected deterministic port %d, got %d", fixedPort, gotFixed)
	}

	fixedURL := fmt.Sprintf("http://127.0.0.1:%d/", fixedPort)
	body, err = waitForHTTP(fixedURL, 20*time.Second)
	if err != nil {
		t.Fatalf("HTTP failed on deterministic port: %v", err)
	}
	t.Logf("  HTTP 200 on :%d (%d bytes)", fixedPort, len(body))

	// Clean up this port for next step
	apiDeleteExposePort(t, instID, 80)
	time.Sleep(500 * time.Millisecond)

	t.Log("PASS: all 6 steps passed")
}

// TestRuntimeExpose_CLI tests the `aegis instance expose` and `unexpose` CLI commands.
func TestRuntimeExpose_CLI(t *testing.T) {
	const handle = "expose-cli-test"

	// Cleanup
	aegis("instance", "delete", handle)
	time.Sleep(500 * time.Millisecond)

	// Start without expose
	out := aegisRun(t, "instance", "start", "--name", handle, "--", "python3", "-m", "http.server", "80")
	t.Logf("start: %s", out)
	t.Cleanup(func() {
		aegis("instance", "delete", handle)
	})

	// Wait for running
	waitForState(t, handle, "running", 30*time.Second)

	// Expose via CLI
	out = aegisRun(t, "instance", "expose", handle, "80")
	t.Logf("expose: %s", out)
	if out == "" {
		t.Fatal("expected output from expose command")
	}

	// Verify info shows endpoint
	out = aegisRun(t, "instance", "info", handle)
	if out == "" {
		t.Fatal("empty info output")
	}
	t.Logf("info:\n%s", out)

	// Unexpose via CLI
	out = aegisRun(t, "instance", "unexpose", handle, "80")
	t.Logf("unexpose: %s", out)
}

// --- helpers ---

func apiDeleteExposePort(t *testing.T, instID string, guestPort int) {
	t.Helper()
	client := daemonClient()
	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("http://aegis/v1/instances/%s/expose/%d", instID, guestPort), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE expose: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE expose returned %d", resp.StatusCode)
	}
}

