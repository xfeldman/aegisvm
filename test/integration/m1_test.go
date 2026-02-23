//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// M1: Serve mode + Router + Lifecycle

func TestServeBasicHTTP(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-m", "http.server", "80"},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Expose port at runtime
	apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", id), map[string]interface{}{"port": 80})

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
		"command": []string{"python3", "-m", "http.server", "80"},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", id), map[string]interface{}{"port": 80})

	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}

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
		"command": []string{"python3", "-m", "http.server", "80"},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", id), map[string]interface{}{"port": 80})

	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}
	t.Log("server ready, waiting 70s for idle pause...")

	time.Sleep(70 * time.Second)

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
		"command": []string{"python3", "-m", "http.server", "80"},
	})

	id := inst["id"].(string)

	apiPost(t, fmt.Sprintf("/v1/instances/%s/expose", id), map[string]interface{}{"port": 80})

	_, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}

	apiDelete(t, "/v1/instances/"+id)

	time.Sleep(1 * time.Second)
	_, err = waitForHTTP("http://127.0.0.1:8099/", 3*time.Second)
	if err == nil {
		t.Fatal("expected router to fail after instance deleted, but it succeeded")
	}
}

func TestRunModeUnchanged(t *testing.T) {
	out := aegisRun(t, "run", "--", "echo", "run mode ok")
	if !strings.Contains(out, "run mode ok") {
		t.Fatalf("run mode broken after serve tests, got: %s", out)
	}
}
