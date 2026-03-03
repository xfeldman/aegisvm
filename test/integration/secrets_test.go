//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDynamicSecrets_CreateWithSecret creates an instance with --secret,
// verifies the secret is available inside the VM via env.
func TestDynamicSecrets_CreateWithSecret(t *testing.T) {
	// Seed a secret
	aegisRun(t, "secret", "set", "TEST_SECRET", "hello-from-secret")
	t.Cleanup(func() { aegis("secret", "delete", "TEST_SECRET") })

	// Create instance with the secret
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sh", "-c", "sleep 600"},
		"image_ref": "python:3.12-alpine",
		"handle":  "secret-test",
		"secrets": []string{"TEST_SECRET"},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	// Wait for running
	waitForState(t, id, "running", 60*time.Second)

	// Exec inside and check env
	out := aegisRun(t, "exec", "secret-test", "--", "sh", "-c", "echo $TEST_SECRET")
	if !strings.Contains(out, "hello-from-secret") {
		t.Fatalf("expected 'hello-from-secret' in exec output, got: %s", out)
	}
}

// TestDynamicSecrets_RotateAndRestart verifies that rotating a secret value
// and restarting the instance picks up the new value.
func TestDynamicSecrets_RotateAndRestart(t *testing.T) {
	// Seed initial secret
	aegisRun(t, "secret", "set", "ROTATE_KEY", "value-v1")
	t.Cleanup(func() { aegis("secret", "delete", "ROTATE_KEY") })

	// Create instance
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sh", "-c", "sleep 600"},
		"image_ref": "python:3.12-alpine",
		"handle":  "rotate-test",
		"secrets": []string{"ROTATE_KEY"},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Verify initial value
	out := aegisRun(t, "exec", "rotate-test", "--", "sh", "-c", "echo $ROTATE_KEY")
	if !strings.Contains(out, "value-v1") {
		t.Fatalf("expected 'value-v1', got: %s", out)
	}

	// Rotate the secret
	aegisRun(t, "secret", "set", "ROTATE_KEY", "value-v2")

	// Disable and re-enable (restart)
	apiPost(t, fmt.Sprintf("/v1/instances/%s/disable", id), nil)
	waitForState(t, id, "stopped", 30*time.Second)

	apiPost(t, fmt.Sprintf("/v1/instances/%s/start", id), nil)
	waitForState(t, id, "running", 60*time.Second)

	// Verify rotated value
	out = aegisRun(t, "exec", "rotate-test", "--", "sh", "-c", "echo $ROTATE_KEY")
	if !strings.Contains(out, "value-v2") {
		t.Fatalf("expected 'value-v2' after rotation, got: %s", out)
	}
}

// TestDynamicSecrets_AddAfterCreation verifies that secrets can be added
// to an instance after creation via PUT /secrets endpoint.
func TestDynamicSecrets_AddAfterCreation(t *testing.T) {
	// Seed secrets
	aegisRun(t, "secret", "set", "KEY_A", "alpha")
	aegisRun(t, "secret", "set", "KEY_B", "bravo")
	t.Cleanup(func() {
		aegis("secret", "delete", "KEY_A")
		aegis("secret", "delete", "KEY_B")
	})

	// Create instance with only KEY_A
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sh", "-c", "sleep 600"},
		"image_ref": "python:3.12-alpine",
		"handle":  "add-secret-test",
		"secrets": []string{"KEY_A"},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 60*time.Second)

	// Verify KEY_A is present, KEY_B is not
	out := aegisRun(t, "exec", "add-secret-test", "--", "sh", "-c", "echo A=$KEY_A B=$KEY_B")
	if !strings.Contains(out, "A=alpha") {
		t.Fatalf("expected 'A=alpha', got: %s", out)
	}
	if strings.Contains(out, "B=bravo") {
		t.Fatalf("KEY_B should not be present yet, got: %s", out)
	}

	// Add KEY_B via API
	apiPut(t, fmt.Sprintf("/v1/instances/%s/secrets", id), map[string]interface{}{
		"secrets": []string{"KEY_A", "KEY_B"},
	})

	// Verify instance info shows both keys
	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	keys, ok := info["secret_keys"].([]interface{})
	if !ok || len(keys) != 2 {
		t.Fatalf("expected 2 secret_keys in info, got: %v", info["secret_keys"])
	}

	// Restart to pick up the new secret
	apiPost(t, fmt.Sprintf("/v1/instances/%s/disable", id), nil)
	waitForState(t, id, "stopped", 30*time.Second)

	apiPost(t, fmt.Sprintf("/v1/instances/%s/start", id), nil)
	waitForState(t, id, "running", 60*time.Second)

	// Verify both secrets are present
	out = aegisRun(t, "exec", "add-secret-test", "--", "sh", "-c", "echo A=$KEY_A B=$KEY_B")
	if !strings.Contains(out, "A=alpha") || !strings.Contains(out, "B=bravo") {
		t.Fatalf("expected both secrets after restart, got: %s", out)
	}
}

// TestDynamicSecrets_SecretKeysInInstanceInfo verifies the secret_keys field
// appears in instance info API responses.
func TestDynamicSecrets_SecretKeysInInstanceInfo(t *testing.T) {
	aegisRun(t, "secret", "set", "INFO_KEY", "test")
	t.Cleanup(func() { aegis("secret", "delete", "INFO_KEY") })

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sh", "-c", "sleep 600"},
		"image_ref": "python:3.12-alpine",
		"handle":  "info-secret-test",
		"secrets": []string{"INFO_KEY"},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDelete(t, "/v1/instances/"+id) })

	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	keys, ok := info["secret_keys"].([]interface{})
	if !ok {
		t.Fatalf("expected secret_keys in instance info, got: %v", info)
	}
	if len(keys) != 1 || keys[0].(string) != "INFO_KEY" {
		t.Errorf("secret_keys = %v, want [INFO_KEY]", keys)
	}
}

// TestDynamicSecrets_CLIRestartMergesSecrets verifies that restarting via CLI
// with --secret additively merges new secret keys.
func TestDynamicSecrets_CLIRestartMergesSecrets(t *testing.T) {
	aegisRun(t, "secret", "set", "MERGE_A", "aaa")
	aegisRun(t, "secret", "set", "MERGE_B", "bbb")
	t.Cleanup(func() {
		aegis("secret", "delete", "MERGE_A")
		aegis("secret", "delete", "MERGE_B")
	})

	// Create with MERGE_A only
	out := aegisRun(t, "instance", "start", "--name", "merge-test", "--secret", "MERGE_A", "--", "sh", "-c", "sleep 600")
	_ = out
	t.Cleanup(func() { aegis("instance", "delete", "merge-test") })

	waitForState(t, "merge-test", "running", 60*time.Second)

	// Disable
	apiPost(t, "/v1/instances/merge-test/disable", nil)
	waitForState(t, "merge-test", "stopped", 30*time.Second)

	// Restart with additional --secret MERGE_B (CLI restart path)
	aegisRun(t, "instance", "start", "--name", "merge-test", "--secret", "MERGE_B")
	waitForState(t, "merge-test", "running", 60*time.Second)

	// Check that both secrets are present
	out = aegisRun(t, "exec", "merge-test", "--", "sh", "-c", "echo A=$MERGE_A B=$MERGE_B")
	if !strings.Contains(out, "A=aaa") || !strings.Contains(out, "B=bbb") {
		t.Fatalf("expected both merged secrets, got: %s", out)
	}
}

// waitForState is defined in conformance_test.go — reuse it here.
