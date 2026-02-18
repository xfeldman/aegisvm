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

// M3: Kits + Secrets + Conformance

func TestSecretInjectionTask(t *testing.T) {
	client := daemonClient()

	// Create app
	app := apiPost(t, "/v1/apps", map[string]interface{}{
		"name":    "secret-task-test",
		"image":   "alpine:3.21",
		"command": []string{"echo", "hello"},
	})
	appID := app["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/apps/secret-task-test") })

	// Set a secret
	apiPut(t, fmt.Sprintf("/v1/apps/%s/secrets/MY_SECRET", appID),
		map[string]string{"value": "secret-value-123"})

	// Run task that prints the env var
	body := map[string]interface{}{
		"command": []string{"sh", "-c", "echo $MY_SECRET"},
		"app_id":  appID,
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

	// Wait for task to finish and collect logs
	out := waitForTaskOutput(t, client, taskID, 60*time.Second)
	if !strings.Contains(out, "secret-value-123") {
		t.Fatalf("expected secret value in output, got: %s", out)
	}
}

func TestSecretNotLeakedInResponse(t *testing.T) {
	client := daemonClient()

	// Create app
	apiPost(t, "/v1/apps", map[string]interface{}{
		"name":    "secret-leak-test",
		"image":   "alpine:3.21",
		"command": []string{"echo"},
	})
	t.Cleanup(func() { apiDelete(t, "/v1/apps/secret-leak-test") })

	// Set a secret
	apiPut(t, "/v1/apps/secret-leak-test/secrets/HIDDEN_KEY",
		map[string]string{"value": "super-secret-do-not-leak"})

	// List secrets
	resp, err := client.Get("http://aegis/v1/apps/secret-leak-test/secrets")
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Verify the value is not in the response
	if strings.Contains(bodyStr, "super-secret-do-not-leak") {
		t.Fatal("secret value leaked in list response")
	}

	// Verify name IS in the response
	if !strings.Contains(bodyStr, "HIDDEN_KEY") {
		t.Fatal("secret name not in list response")
	}
}

func TestWorkspaceSecrets(t *testing.T) {
	// Set a workspace secret
	apiPut(t, "/v1/secrets/WS_KEY", map[string]string{"value": "workspace-val"})
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/secrets/WS_KEY") })

	// List workspace secrets
	client := daemonClient()
	resp, err := client.Get("http://aegis/v1/secrets")
	if err != nil {
		t.Fatalf("list workspace secrets: %v", err)
	}
	defer resp.Body.Close()

	var secrets []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&secrets)

	found := false
	for _, sec := range secrets {
		if sec["name"] == "WS_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("workspace secret WS_KEY not found in list")
	}
}

func TestKitInstallListInfo(t *testing.T) {
	client := daemonClient()

	// Register a kit via API
	kit := apiPost(t, "/v1/kits", map[string]interface{}{
		"name":        "test-kit",
		"version":     "1.0.0",
		"description": "A test kit",
		"image_ref":   "test-image:latest",
		"config":      map[string]interface{}{},
	})
	if kit["name"] != "test-kit" {
		t.Fatalf("expected name test-kit, got %v", kit["name"])
	}

	// List kits
	resp, err := client.Get("http://aegis/v1/kits")
	if err != nil {
		t.Fatalf("list kits: %v", err)
	}
	defer resp.Body.Close()

	var kits []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&kits)

	found := false
	for _, k := range kits {
		if k["name"] == "test-kit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("test-kit not found in list")
	}

	// Get kit info
	resp2, err := client.Get("http://aegis/v1/kits/test-kit")
	if err != nil {
		t.Fatalf("get kit: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != 200 {
		t.Fatalf("get kit returned %d", resp2.StatusCode)
	}

	// Delete kit
	apiDelete(t, "/v1/kits/test-kit")

	// Verify deleted
	resp3, err := client.Get("http://aegis/v1/kits/test-kit")
	if err != nil {
		t.Fatalf("get deleted kit: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp3.StatusCode)
	}
}

func TestDoctorCapabilities(t *testing.T) {
	out, err := aegis("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\noutput: %s", err, out)
	}

	// When daemon is running, should show capabilities
	if !strings.Contains(out, "Backend:") {
		t.Fatalf("expected Backend in doctor output, got: %s", out)
	}
	if !strings.Contains(out, "Pause/Resume:") {
		t.Fatalf("expected Pause/Resume in doctor output, got: %s", out)
	}
	if !strings.Contains(out, "Installed kits:") {
		t.Fatalf("expected Installed kits in doctor output, got: %s", out)
	}
}
