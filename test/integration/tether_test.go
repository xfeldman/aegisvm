//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestTetherRoundTrip verifies the full tether pipeline:
// gateway API → aegisd → harness → agent → harness → aegisd → egress stream.
//
// No LLM API key is injected, so the agent responds with a fallback message.
// This exercises tether ingress, harness forwarding, agent processing,
// tether egress, and the stream endpoint.
func TestTetherRoundTrip(t *testing.T) {
	handle := "tether-test"

	// Clean up any previous run
	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(500 * time.Millisecond)

	// Create agent instance with OCI image (triggers InjectGuestBinaries).
	// No LLM key — agent will use fallback response.
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"aegis-agent"},
		"handle":    handle,
		"image_ref": "python:3.12-alpine",
		"workspace": "tether-test",
	})
	id := inst["id"].(string)
	t.Cleanup(func() {
		apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", id))
	})

	// Wait for instance to be running
	waitForState(t, id, "running", 30*time.Second)

	// Wait a moment for agent HTTP server to start inside VM
	time.Sleep(2 * time.Second)

	// Send tether frame (ingress)
	client := daemonClient()
	frame := map[string]interface{}{
		"v":       1,
		"type":    "user.message",
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"session": map[string]string{"channel": "test", "id": "integration-1"},
		"msg_id":  "test-001",
		"seq":     1,
		"payload": map[string]interface{}{
			"text": "Hello agent",
			"user": map[string]string{"id": "1", "name": "TestUser"},
		},
	}
	frameJSON, _ := json.Marshal(frame)
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/tether", id),
		"application/json",
		bytes.NewReader(frameJSON),
	)
	if err != nil {
		t.Fatalf("POST tether: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	// Wait for agent to process and send egress frames
	time.Sleep(3 * time.Second)

	// Read egress stream
	streamResp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/tether/stream", id))
	if err != nil {
		t.Fatalf("GET tether/stream: %v", err)
	}

	// Read with a short timeout — we just need the buffered frames
	done := make(chan []map[string]interface{}, 1)
	go func() {
		var frames []map[string]interface{}
		scanner := bufio.NewScanner(streamResp.Body)
		for scanner.Scan() {
			var f map[string]interface{}
			if json.Unmarshal(scanner.Bytes(), &f) == nil {
				frames = append(frames, f)
			}
			// Stop after we see assistant.done
			if fType, _ := f["type"].(string); fType == "assistant.done" {
				break
			}
		}
		done <- frames
	}()

	var frames []map[string]interface{}
	select {
	case frames = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for egress frames")
	}
	streamResp.Body.Close()

	if len(frames) == 0 {
		t.Fatal("no egress frames received")
	}

	// Verify we got status.presence and assistant.done
	var gotPresence, gotDone bool
	var doneText string
	for _, f := range frames {
		switch f["type"] {
		case "status.presence":
			gotPresence = true
		case "assistant.done":
			gotDone = true
			if payload, ok := f["payload"].(map[string]interface{}); ok {
				doneText, _ = payload["text"].(string)
			}
		}
	}

	if !gotPresence {
		t.Error("missing status.presence frame")
	}
	if !gotDone {
		t.Fatal("missing assistant.done frame")
	}

	// Without an API key, agent responds with the fallback message
	if !strings.Contains(doneText, "No LLM API key") {
		t.Logf("unexpected done text (may have LLM key configured): %s", doneText)
	}

	t.Logf("tether round-trip OK: %d frames, done text: %q", len(frames), doneText)
}

// TestTetherUserIdentity verifies that user info is preserved in the session.
func TestTetherUserIdentity(t *testing.T) {
	handle := "tether-user-test"

	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(500 * time.Millisecond)

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"aegis-agent"},
		"handle":    handle,
		"image_ref": "python:3.12-alpine",
		"workspace": "tether-user-test",
	})
	id := inst["id"].(string)
	t.Cleanup(func() {
		apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", id))
	})

	waitForState(t, id, "running", 30*time.Second)
	time.Sleep(2 * time.Second)

	// Send message with user info
	client := daemonClient()
	frame := map[string]interface{}{
		"v":       1,
		"type":    "user.message",
		"session": map[string]string{"channel": "test", "id": "user-test-1"},
		"payload": map[string]interface{}{
			"text": "hello from group",
			"user": map[string]string{
				"id":       "42",
				"username": "testuser",
				"name":     "Alice",
			},
		},
	}
	frameJSON, _ := json.Marshal(frame)
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/tether", id),
		"application/json",
		bytes.NewReader(frameJSON),
	)
	if err != nil {
		t.Fatalf("POST tether: %v", err)
	}
	resp.Body.Close()

	// Wait for processing
	time.Sleep(3 * time.Second)

	// Verify session file was written with user attribution
	execResult := apiPost(t, fmt.Sprintf("/v1/instances/%s/exec", id), map[string]interface{}{
		"command": []string{"cat", "/workspace/sessions/test_user-test-1.jsonl"},
	})
	_ = execResult

	// Read the exec output from logs
	logsResp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs?tail=20", id))
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer logsResp.Body.Close()
	logsBody, _ := io.ReadAll(logsResp.Body)

	// Check that the session contains [Alice] attribution
	if !strings.Contains(string(logsBody), "[Alice]") {
		t.Logf("logs: %s", logsBody)
		t.Error("session does not contain user attribution [Alice]")
	}

	t.Log("user identity in tether OK")
}

