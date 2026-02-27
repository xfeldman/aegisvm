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

// Backend conformance: exercises the core instance lifecycle paths that any
// VMM backend must support. These tests run against whatever backend aegisd
// was started with (libkrun on macOS, Cloud Hypervisor on Linux).

// TestConformance_RunEcho — boot VM, run command, verify output, instance stops.
func TestConformance_RunEcho(t *testing.T) {
	out := aegisRun(t, "run", "--", "echo", "conformance-echo")
	if !strings.Contains(out, "conformance-echo") {
		t.Fatalf("expected 'conformance-echo' in output, got: %s", out)
	}
}

// TestConformance_RunExitCode — verify non-zero exit code propagation.
func TestConformance_RunExitCode(t *testing.T) {
	_, err := aegis("run", "--", "sh", "-c", "exit 42")
	if err == nil {
		t.Fatal("expected non-zero exit, got success")
	}
}

// TestConformance_InstanceStartStop — start with API, verify running, stop, verify gone.
func TestConformance_InstanceStartStop(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-startstop",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	// Wait for RUNNING
	waitForState(t, id, "running", 30*time.Second)

	// Stop
	apiDelete(t, "/v1/instances/"+id)

	// Verify gone from list
	client := daemonClient()
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", id))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("instance should be gone after delete")
		}
	}
}

// TestConformance_InstanceList — start instance, verify it appears in list with handle.
func TestConformance_InstanceList(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-list",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	client := daemonClient()
	resp, err := client.Get("http://aegis/v1/instances")
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var instances []map[string]interface{}
	json.Unmarshal(data, &instances)

	found := false
	for _, inst := range instances {
		if inst["handle"] == "conf-list" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("instance with handle 'conf-list' not found in list: %s", data)
	}
}

// TestConformance_Exec — start instance, exec a command, verify output + done marker.
func TestConformance_Exec(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-exec",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	// Exec
	client := daemonClient()
	body := `{"command":["echo","exec-output"]}`
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/exec", id),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("exec returned %d: %s", resp.StatusCode, data)
	}

	// Read NDJSON stream
	decoder := json.NewDecoder(resp.Body)
	gotOutput := false
	gotDone := false
	for decoder.More() {
		var entry map[string]interface{}
		if err := decoder.Decode(&entry); err != nil {
			break
		}
		if line, ok := entry["line"].(string); ok && strings.Contains(line, "exec-output") {
			gotOutput = true
		}
		if done, ok := entry["done"].(bool); ok && done {
			gotDone = true
		}
	}

	if !gotOutput {
		t.Fatal("did not see exec output in NDJSON stream")
	}
	if !gotDone {
		t.Fatal("did not see done marker in NDJSON stream")
	}
}

// TestConformance_Logs — start instance, generate output, verify logs contain it.
func TestConformance_Logs(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo log-test-line && sleep 300"},
		"handle":  "conf-logs",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	// Give the echo time to appear in logs
	time.Sleep(2 * time.Second)

	client := daemonClient()
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "log-test-line") {
		t.Fatalf("expected 'log-test-line' in logs, got: %.500s", data)
	}
}

// TestConformance_LogsExecSource — exec into instance, verify exec_id on log entries.
func TestConformance_LogsExecSource(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-logsrc",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	// Exec
	client := daemonClient()
	body := `{"command":["echo","exec-src-line"]}`
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/exec", id),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	// Drain the exec response
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// Read instance logs, look for exec source
	time.Sleep(1 * time.Second)
	logsResp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	defer logsResp.Body.Close()

	decoder := json.NewDecoder(logsResp.Body)
	foundExecSource := false
	for decoder.More() {
		var entry map[string]interface{}
		if err := decoder.Decode(&entry); err != nil {
			break
		}
		if entry["source"] == "exec" && entry["exec_id"] != nil && entry["exec_id"] != "" {
			foundExecSource = true
			break
		}
	}
	if !foundExecSource {
		t.Fatal("no log entry with source=exec and exec_id found")
	}
}

// TestConformance_ProcessExited — start short-lived command, verify instance transitions to stopped.
func TestConformance_ProcessExited(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"echo", "done"},
		"handle":  "conf-exit",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	// Wait for instance to transition through running → stopped
	waitForState(t, id, "stopped", 30*time.Second)

	// Verify system log entry
	time.Sleep(500 * time.Millisecond)
	client := daemonClient()
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "process exited") {
		t.Fatalf("expected 'process exited' system log, got: %.500s", data)
	}
}

// TestConformance_ExecStopped409 — exec on stopped instance returns 409.
func TestConformance_ExecStopped409(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"echo", "quick"},
		"handle":  "conf-exec409",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	// Wait for process to exit → stopped
	waitForState(t, id, "stopped", 30*time.Second)

	// Exec should fail with 409
	client := daemonClient()
	body := `{"command":["echo","fail"]}`
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/exec", id),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("exec request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 for exec on stopped instance, got %d", resp.StatusCode)
	}
}

// TestConformance_HandleResolution — create with handle, resolve by handle for exec and logs.
func TestConformance_HandleResolution(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-handle",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	// Get by handle
	info := apiGet(t, "/v1/instances/conf-handle")
	if info["id"] != id {
		t.Fatalf("handle resolution: got id %v, want %s", info["id"], id)
	}

	// Exec by handle
	client := daemonClient()
	body := `{"command":["echo","handle-exec"]}`
	resp, err := client.Post(
		"http://aegis/v1/instances/conf-handle/exec",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("exec by handle: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("exec by handle returned %d", resp.StatusCode)
	}
}

// TestConformance_PauseResume — explicit pause/resume via API.
func TestConformance_PauseResume(t *testing.T) {
	if testing.Short() {
		t.Skip("pause/resume test skipped in short mode")
	}

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sleep", "300"},
		"handle":  "conf-pr",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 30*time.Second)

	// Pause
	apiPost(t, fmt.Sprintf("/v1/instances/%s/pause", id), nil)

	info := apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	if info["state"] != "paused" {
		t.Fatalf("expected paused, got %v", info["state"])
	}

	// Resume
	apiPost(t, fmt.Sprintf("/v1/instances/%s/resume", id), nil)

	info = apiGet(t, fmt.Sprintf("/v1/instances/%s", id))
	if info["state"] != "running" {
		t.Fatalf("expected running after resume, got %v", info["state"])
	}
}

// TestConformance_CLIRun — aegis run -- echo hello exits with 0.
func TestConformance_CLIRun(t *testing.T) {
	out := aegisRun(t, "run", "--", "echo", "cli-run-test")
	if !strings.Contains(out, "cli-run-test") {
		t.Fatalf("expected 'cli-run-test', got: %s", out)
	}
}

// TestConformance_SecretSetListDelete — CRUD lifecycle for secrets.
func TestConformance_SecretSetListDelete(t *testing.T) {
	// Set two secrets
	apiPut(t, "/v1/secrets/CONF_KEY_A", map[string]interface{}{"value": "alpha"})
	apiPut(t, "/v1/secrets/CONF_KEY_B", map[string]interface{}{"value": "beta"})

	// List — both should appear
	client := daemonClient()
	resp, err := client.Get("http://aegis/v1/secrets")
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(data), "CONF_KEY_A") || !strings.Contains(string(data), "CONF_KEY_B") {
		t.Fatalf("expected both secrets in list, got: %s", data)
	}

	// Delete one
	apiDelete(t, "/v1/secrets/CONF_KEY_A")

	// List again — only B should remain
	resp2, _ := client.Get("http://aegis/v1/secrets")
	defer resp2.Body.Close()
	data2, _ := io.ReadAll(resp2.Body)

	if strings.Contains(string(data2), "CONF_KEY_A") {
		t.Fatal("CONF_KEY_A should be deleted")
	}
	if !strings.Contains(string(data2), "CONF_KEY_B") {
		t.Fatal("CONF_KEY_B should still exist")
	}

	// Clean up
	apiDeleteAllowFail(t, "/v1/secrets/CONF_KEY_B")
}

// TestConformance_SecretScopingNone — default (no --env KEY) injects nothing.
func TestConformance_SecretScopingNone(t *testing.T) {
	// Set a secret
	apiPut(t, "/v1/secrets/SCOPE_TEST", map[string]interface{}{"value": "should-not-appear"})
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/secrets/SCOPE_TEST") })

	// Start instance with NO secrets field (default = inject none)
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo SECRET=$SCOPE_TEST"},
		"handle":  "conf-scope-none",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	// Wait for process to exit (short-lived)
	waitForState(t, id, "stopped", 30*time.Second)

	// Check logs — SCOPE_TEST should be empty
	time.Sleep(500 * time.Millisecond)
	client := daemonClient()
	resp, _ := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(data), "should-not-appear") {
		t.Fatal("secret value leaked without --env flag")
	}
	if !strings.Contains(string(data), "SECRET=") {
		t.Fatal("expected 'SECRET=' (empty) in output")
	}
}

// TestConformance_SecretScopingAllowlist — only named secrets are injected.
func TestConformance_SecretScopingAllowlist(t *testing.T) {
	apiPut(t, "/v1/secrets/WANTED_KEY", map[string]interface{}{"value": "wanted-val"})
	apiPut(t, "/v1/secrets/UNWANTED_KEY", map[string]interface{}{"value": "unwanted-val"})
	t.Cleanup(func() {
		apiDeleteAllowFail(t, "/v1/secrets/WANTED_KEY")
		apiDeleteAllowFail(t, "/v1/secrets/UNWANTED_KEY")
	})

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo WANTED=$WANTED_KEY UNWANTED=$UNWANTED_KEY"},
		"secrets": []string{"WANTED_KEY"},
		"handle":  "conf-scope-allow",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "stopped", 30*time.Second)

	time.Sleep(500 * time.Millisecond)
	client := daemonClient()
	resp, _ := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	output := string(data)

	if !strings.Contains(output, "wanted-val") {
		t.Fatalf("expected WANTED_KEY value in output, got: %.500s", output)
	}
	if strings.Contains(output, "unwanted-val") {
		t.Fatal("UNWANTED_KEY value leaked despite not being in allowlist")
	}
}

// TestConformance_SecretScopingAll — ["*"] injects all secrets.
func TestConformance_SecretScopingAll(t *testing.T) {
	apiPut(t, "/v1/secrets/ALL_A", map[string]interface{}{"value": "val-a"})
	apiPut(t, "/v1/secrets/ALL_B", map[string]interface{}{"value": "val-b"})
	t.Cleanup(func() {
		apiDeleteAllowFail(t, "/v1/secrets/ALL_A")
		apiDeleteAllowFail(t, "/v1/secrets/ALL_B")
	})

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo A=$ALL_A B=$ALL_B"},
		"secrets": []string{"*"},
		"handle":  "conf-scope-all",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "stopped", 30*time.Second)

	time.Sleep(500 * time.Millisecond)
	client := daemonClient()
	resp, _ := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	output := string(data)

	if !strings.Contains(output, "val-a") || !strings.Contains(output, "val-b") {
		t.Fatalf("expected both secret values with '*', got: %.500s", output)
	}
}

// TestConformance_SecretEnvOverride — explicit --env overrides secret with same name.
func TestConformance_SecretEnvOverride(t *testing.T) {
	apiPut(t, "/v1/secrets/OVERRIDE_KEY", map[string]interface{}{"value": "from-store"})
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/secrets/OVERRIDE_KEY") })

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"sh", "-c", "echo VAL=$OVERRIDE_KEY"},
		"secrets": []string{"OVERRIDE_KEY"},
		"env":     map[string]string{"OVERRIDE_KEY": "from-env"},
		"handle":  "conf-override",
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "stopped", 30*time.Second)

	time.Sleep(500 * time.Millisecond)
	client := daemonClient()
	resp, _ := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs", id))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	output := string(data)

	if !strings.Contains(output, "from-env") {
		t.Fatalf("expected explicit env override, got: %.500s", output)
	}
	if strings.Contains(output, "from-store") {
		t.Fatal("secret value should be overridden by explicit env")
	}
}

// waitForState polls instance state until it matches or timeout.
func waitForState(t *testing.T, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := daemonClient()
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", id))
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var inst map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&inst)
		resp.Body.Close()
		if state, _ := inst["state"].(string); state == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("instance %s did not reach state %q within %v", id, want, timeout)
}
