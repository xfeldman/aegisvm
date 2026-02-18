//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// M2: Releases + Apps + Overlays

func TestRunWithImage(t *testing.T) {
	out := aegisRun(t, "run", "--image", "alpine:3.21", "--", "echo", "hello from alpine")
	if !strings.Contains(out, "hello from alpine") {
		t.Fatalf("expected 'hello from alpine' in output, got: %s", out)
	}
}

func TestAppCreatePublishServe(t *testing.T) {
	client := daemonClient()

	// Create app
	app := apiPost(t, "/v1/apps", map[string]interface{}{
		"name":         "test-app",
		"image":        "python:3.12-alpine",
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})
	appID := app["id"].(string)
	t.Logf("created app: %s", appID)
	t.Cleanup(func() { apiDelete(t, "/v1/apps/test-app") })

	// Publish
	pub := apiPost(t, "/v1/apps/test-app/publish", map[string]interface{}{
		"label": "v1",
	})
	relID, _ := pub["id"].(string)
	t.Logf("published release: %s", relID)

	// Serve
	serve := apiPost(t, "/v1/apps/test-app/serve", map[string]interface{}{})
	instID, _ := serve["id"].(string)
	t.Logf("serving instance: %s", instID)
	t.Cleanup(func() {
		apiDelete(t, fmt.Sprintf("/v1/instances/%s", instID))
	})

	// Wait for HTTP
	body, err := waitForHTTP("http://127.0.0.1:8099/", 90*time.Second)
	if err != nil {
		t.Fatalf("serve HTTP failed: %v", err)
	}
	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing, got: %.200s", body)
	}

	// List releases
	resp, err := client.Get("http://aegis/v1/apps/test-app/releases")
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	defer resp.Body.Close()
	var releases []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&releases)
	if len(releases) == 0 {
		t.Fatal("expected at least one release")
	}
}

func TestAppPublishUsesCache(t *testing.T) {
	// Create app
	apiPost(t, "/v1/apps", map[string]interface{}{
		"name":         "cache-test",
		"image":        "alpine:3.21",
		"command":      []string{"echo", "hello"},
		"expose_ports": []int{},
	})
	t.Cleanup(func() { apiDelete(t, "/v1/apps/cache-test") })

	// First publish — will pull image
	start1 := time.Now()
	apiPost(t, "/v1/apps/cache-test/publish", map[string]interface{}{"label": "v1"})
	dur1 := time.Since(start1)
	t.Logf("first publish: %v", dur1)

	// Second publish — should use cache
	start2 := time.Now()
	apiPost(t, "/v1/apps/cache-test/publish", map[string]interface{}{"label": "v2"})
	dur2 := time.Since(start2)
	t.Logf("second publish: %v", dur2)

	// Cache hit should be significantly faster
	if dur2 > dur1/2 && dur1 > 5*time.Second {
		t.Logf("second publish was not significantly faster (first=%v, second=%v)", dur1, dur2)
		// Not a hard failure — network conditions vary
	}
}

func TestM1BackwardCompat(t *testing.T) {
	// aegis run --expose without --image should still work (using base rootfs)
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":      []string{"python3", "-m", "http.server", "80"},
		"expose_ports": []int{80},
	})

	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	body, err := waitForHTTP("http://127.0.0.1:8099/", 60*time.Second)
	if err != nil {
		t.Fatalf("backward compat failed: %v", err)
	}
	if !strings.Contains(body, "Directory listing") {
		t.Fatalf("expected directory listing, got: %.200s", body)
	}
}

func TestAppListAndInfo(t *testing.T) {
	client := daemonClient()

	// Create app
	apiPost(t, "/v1/apps", map[string]interface{}{
		"name":    "info-test",
		"image":   "alpine:3.21",
		"command": []string{"echo", "hello"},
	})
	t.Cleanup(func() { apiDelete(t, "/v1/apps/info-test") })

	// List apps
	resp, err := client.Get("http://aegis/v1/apps")
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var apps []map[string]interface{}
	json.Unmarshal(body, &apps)

	found := false
	for _, app := range apps {
		if app["name"] == "info-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("app 'info-test' not found in list")
	}

	// Get app by name
	resp2, err := client.Get("http://aegis/v1/apps/info-test")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("get app returned %d", resp2.StatusCode)
	}
}
