//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestGuestMCP_Initialize tests that the MCP guest server responds to initialize.
func TestGuestMCP_Initialize(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-init-test",
		"command": []string{"sleep", "120"},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	waitForState(t, id, "running", 30*time.Second)

	// Send initialize via MCP protocol
	out := aegisExec(t, id, "sh", "-c",
		`echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}' | aegis-mcp-guest`)

	if !strings.Contains(out, "aegis-guest") {
		t.Fatalf("initialize should return server info with name 'aegis-guest', got: %s", out)
	}
	if !strings.Contains(out, "instance_spawn") {
		t.Fatalf("initialize should mention tools in instructions, got: %s", out)
	}
}

// TestGuestMCP_ToolsList tests that tools/list returns all 6 tools.
func TestGuestMCP_ToolsList(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-tools-test",
		"command": []string{"sleep", "120"},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	waitForState(t, id, "running", 30*time.Second)

	out := aegisExec(t, id, "sh", "-c",
		`printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/list"}\n' | aegis-mcp-guest | tail -1`)

	// Parse the tools/list response
	var mcpResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &mcpResp); err != nil {
		t.Fatalf("failed to parse tools/list response: %v\noutput: %s", err, out)
	}

	expectedTools := []string{"instance_spawn", "instance_list", "instance_stop", "self_info", "keepalive_acquire", "keepalive_release"}
	if len(mcpResp.Result.Tools) != len(expectedTools) {
		t.Fatalf("expected %d tools, got %d: %s", len(expectedTools), len(mcpResp.Result.Tools), out)
	}

	toolNames := make(map[string]bool)
	for _, tool := range mcpResp.Result.Tools {
		toolNames[tool.Name] = true
	}
	for _, expected := range expectedTools {
		if !toolNames[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

// TestGuestMCP_SelfInfo tests calling self_info via MCP tools/call.
func TestGuestMCP_SelfInfo(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-self-test",
		"command": []string{"sleep", "120"},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	waitForState(t, id, "running", 30*time.Second)

	out := aegisExec(t, id, "sh", "-c",
		`printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"self_info","arguments":{}}}\n' | aegis-mcp-guest | tail -1`)

	if !strings.Contains(out, id) {
		t.Fatalf("self_info should return instance ID %s, got: %s", id, out)
	}
	if !strings.Contains(out, "mcp-self-test") {
		t.Fatalf("self_info should return handle, got: %s", out)
	}
}

// TestGuestMCP_SpawnViaTools tests spawning a child via MCP tools/call.
func TestGuestMCP_SpawnViaTools(t *testing.T) {
	resp := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-spawn-test",
		"command": []string{"sleep", "120"},
		"capabilities": map[string]interface{}{
			"spawn":          true,
			"spawn_depth":    1,
			"max_children":   3,
			"allowed_images": []string{"*"},
			"max_memory_mb":  1024,
		},
	})
	id := resp["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id)

	waitForState(t, id, "running", 30*time.Second)

	out := aegisExec(t, id, "sh", "-c",
		`printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"instance_spawn","arguments":{"command":["echo","mcp-child"],"handle":"mcp-child-1"}}}\n' | aegis-mcp-guest | tail -1`)

	if !strings.Contains(out, "mcp-child-1") {
		t.Fatalf("spawn should return child handle, got: %s", out)
	}
	if !strings.Contains(out, id) {
		t.Fatalf("spawn should return parent_id, got: %s", out)
	}
}

// TestGuestMCP_InstanceListScoped tests that instance_list only shows own children.
func TestGuestMCP_InstanceListScoped(t *testing.T) {
	// Create two parent instances
	resp1 := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-parent-a",
		"command": []string{"sleep", "120"},
		"capabilities": map[string]interface{}{
			"spawn": true, "spawn_depth": 1, "max_children": 3, "allowed_images": []string{"*"}, "max_memory_mb": 1024,
		},
	})
	id1 := resp1["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id1)

	resp2 := apiPost(t, "/v1/instances", map[string]interface{}{
		"handle":  "mcp-parent-b",
		"command": []string{"sleep", "120"},
		"capabilities": map[string]interface{}{
			"spawn": true, "spawn_depth": 1, "max_children": 3, "allowed_images": []string{"*"}, "max_memory_mb": 1024,
		},
	})
	id2 := resp2["id"].(string)
	defer apiDelete(t, "/v1/instances/"+id2)

	waitForState(t, id1, "running", 30*time.Second)
	waitForState(t, id2, "running", 30*time.Second)

	// Spawn a child from parent A
	aegisExec(t, id1, "sh", "-c",
		`printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"instance_spawn","arguments":{"command":["sleep","60"],"handle":"child-of-a"}}}\n' | aegis-mcp-guest > /dev/null`)

	time.Sleep(2 * time.Second)

	// List children from parent B — should NOT see child-of-a
	out := aegisExec(t, id2, "sh", "-c",
		`printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"instance_list","arguments":{}}}\n' | aegis-mcp-guest | tail -1`)

	if strings.Contains(out, "child-of-a") {
		t.Fatalf("parent B should NOT see parent A's children, got: %s", out)
	}
}
