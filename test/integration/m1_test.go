//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// M1: Serve mode + Router + Lifecycle

func TestServeBasicHTTP(t *testing.T) {
	// Create a serve instance via the API
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for the server to become ready and the router to proxy
	body, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("serve mode HTTP failed: %v", err)
	}

	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing from python http.server, got: %.200s", body)
	}
}

func TestServeMultipleCurls(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for ready
	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}

	// Multiple sequential requests should all succeed
	for i := 0; i < 5; i++ {
		body, err := waitForHTTP("http://127.0.0.1:8099/", 10*time.Second)
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if !strings.Contains(body, "Directory listing") {
			t.Fatalf("request %d: unexpected body: %.200s", i+1, body)
		}
	}
}

func TestServePauseResume(t *testing.T) {
	if testing.Short() {
		t.Skip("pause/resume test requires 70s+ idle wait")
	}

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for server ready
	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}
	t.Log("server ready, waiting 70s for idle pause...")

	// Wait for idle timeout (default 60s) + buffer
	time.Sleep(70 * time.Second)

	// Curl should wake the VM and succeed
	t.Log("sending wake request...")
	body, err := waitForHTTP("http://127.0.0.1:8099/", 30*time.Second)
	if err != nil {
		t.Fatalf("request after pause failed: %v", err)
	}

	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing after wake, got: %.200s", body)
	}
	t.Log("VM woke from pause successfully")
}

func TestServeCleanShutdown(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})

	id := inst["id"].(string)

	// Wait for ready
	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}

	// Delete should succeed
	apiDelete(t, "/v1/instances/"+id)

	// Router should now return error (no instance)
	time.Sleep(1 * time.Second)
	_, err = waitForHTTP("http://127.0.0.1:8099/", 3*time.Second)
	if err == nil {
		t.Fatal("expected router to fail after instance deleted, but it succeeded")
	}
}

func TestTaskModeUnchanged(t *testing.T) {
	// After running serve tests, task mode should still work
	out := aegisRun(t, "run", "--", "echo", "task mode ok")
	if !strings.Contains(out, "task mode ok") {
		t.Fatalf("task mode broken after serve tests, got: %s", out)
	}
}
