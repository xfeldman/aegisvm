// aegis-mcp-guest is an MCP server that runs inside Aegis VMs, allowing
// LLM agents to spawn and manage child instances via the Guest Orchestration API.
//
// It communicates over stdio (JSON-RPC 2.0) and calls the Guest API on
// http://127.0.0.1:7777. No authentication is needed — the harness handles it.
//
// This binary is included in the VM rootfs alongside the harness.
// Agents invoke it via: aegis-mcp-guest (stdin/stdout MCP protocol)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/xfeldman/aegisvm/internal/version"
)

// Guest API base URL (harness-provided HTTP server inside the VM)
const guestAPIBase = "http://127.0.0.1:7777"

var httpClient = &http.Client{}

// --- MCP protocol types ---

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools *struct{} `json:"tools,omitempty"`
}

type mcpInitResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	Instructions    string          `json:"instructions"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// --- Tool definitions ---

var tools = []mcpTool{
	{
		Name:        "instance_spawn",
		Description: `Spawn a new child VM instance. The child is fully isolated — it cannot see your files unless you provide a workspace.

Correct workflow:
1. Create a subdirectory: /workspace/my-app/
2. Write child's files there: /workspace/my-app/server.py
3. Spawn with workspace="/workspace/my-app" and command=["python3", "/workspace/server.py"]

CRITICAL: The workspace directory becomes /workspace/ inside the child. So if you write /workspace/my-app/server.py and set workspace="/workspace/my-app", the child sees it as /workspace/server.py — NOT /workspace/my-app/server.py. Command paths must be relative to /workspace/ in the child.

Children are automatically stopped when the parent stops.`,
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"command":   {"type": "array", "items": {"type": "string"}, "description": "Command to run inside the child. Paths must be relative to the child's /workspace/. Example: if you set workspace='/workspace/my-app', the child sees that directory as /workspace/, so use command=['python3', '/workspace/server.py'] NOT ['python3', '/workspace/my-app/server.py']."},
				"handle":    {"type": "string", "description": "Human-friendly name for the child (e.g. 'calc-app', 'web-server')."},
				"image_ref": {"type": "string", "description": "OCI image for the child VM (e.g. 'node:22', 'python:3.12-alpine'). Default: Alpine Linux."},
				"workspace": {"type": "string", "description": "A subdirectory under /workspace/ that becomes the child's /workspace/. Create and populate it before spawning. Example: '/workspace/my-app'. Do NOT use '/workspace' itself."},
				"exposes":   {"type": "array", "items": {"type": "integer"}, "description": "Guest ports to expose on the host (e.g. [8080]). Response includes public_port."},
				"memory_mb": {"type": "integer", "description": "Child VM memory in MB. Default: 512."},
				"env":       {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variables for the child."}
			},
			"required": ["command"]
		}`),
	},
	{
		Name:        "instance_list",
		Description: "List all child instances spawned by this VM. Returns each child's ID, handle, and current state (starting, running, paused, stopped).",
		InputSchema: rawJSON(`{"type": "object", "properties": {}}`),
	},
	{
		Name:        "instance_stop",
		Description: "Stop a child instance. The child's VM is terminated and its resources freed. Only works on children spawned by this VM.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Child instance ID or handle to stop."}
			},
			"required": ["id"]
		}`),
	},
	{
		Name:        "self_info",
		Description: "Get information about this VM instance — its ID, handle, state, image, parent ID, and endpoints.",
		InputSchema: rawJSON(`{"type": "object", "properties": {}}`),
	},
	{
		Name:        "expose_port",
		Description: "Expose a guest port on the host. Returns the allocated public port. Can be called at any time while the VM is running. Idempotent: re-exposing returns the existing mapping.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"guest_port":  {"type": "integer", "description": "Port inside this VM to expose (e.g. 8080)."},
				"public_port": {"type": "integer", "description": "Specific host port to use. 0 or omitted = random."},
				"protocol":    {"type": "string", "description": "Protocol hint: 'http' (default), 'tcp'."}
			},
			"required": ["guest_port"]
		}`),
	},
	{
		Name:        "unexpose_port",
		Description: "Remove a port exposure. Closes the host listener for that guest port.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"guest_port": {"type": "integer", "description": "Guest port to unexpose."}
			},
			"required": ["guest_port"]
		}`),
	},
	{
		Name:        "keepalive_acquire",
		Description: "Acquire a keepalive lease to prevent this VM from being paused by the idle timer. Use this during long-running work (builds, computations) that doesn't generate network traffic. The lease auto-expires after ttl_ms milliseconds.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"ttl_ms": {"type": "integer", "description": "Lease duration in milliseconds. Default: 30000 (30 seconds). Renew before expiry to keep alive."},
				"reason": {"type": "string", "description": "Why the lease is held (for debugging). Example: 'building project', 'running tests'."}
			}
		}`),
	},
	{
		Name:        "keepalive_release",
		Description: "Release the keepalive lease, allowing the VM to pause when idle.",
		InputSchema: rawJSON(`{"type": "object", "properties": {}}`),
	},
}

// --- Tool handlers ---

func handleSpawn(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("POST", "/v1/instances", args)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("spawn failed (HTTP %d): %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleList(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("GET", "/v1/instances", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("list failed (HTTP %d): %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleStop(args json.RawMessage) *mcpToolResult {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(args, &params)
	if params.ID == "" {
		return errorResult("id is required")
	}
	status, data, err := doRequest("POST", "/v1/instances/"+params.ID+"/stop", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("stop failed (HTTP %d): %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleSelfInfo(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("GET", "/v1/self", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("self_info failed (HTTP %d): %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleKeepaliveAcquire(args json.RawMessage) *mcpToolResult {
	body := args
	if len(body) == 0 || string(body) == "{}" || string(body) == "null" {
		body = []byte(`{"ttl_ms":30000,"reason":"agent"}`)
	}
	status, data, err := doRequest("POST", "/v1/self/keepalive", body)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("keepalive failed (HTTP %d): %s", status, string(data)))
	}
	return textResult("keepalive lease acquired")
}

func handleKeepaliveRelease(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("DELETE", "/v1/self/keepalive", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("release failed (HTTP %d): %s", status, string(data)))
	}
	_ = data
	return textResult("keepalive lease released")
}

func handleExposePort(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("POST", "/v1/self/expose", args)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("expose failed (HTTP %d): %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleUnexposePort(args json.RawMessage) *mcpToolResult {
	var params struct {
		GuestPort int `json:"guest_port"`
	}
	json.Unmarshal(args, &params)
	if params.GuestPort <= 0 {
		return errorResult("guest_port is required")
	}
	status, data, err := doRequest("DELETE", fmt.Sprintf("/v1/self/expose/%d", params.GuestPort), nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("unexpose failed (HTTP %d): %s", status, string(data)))
	}
	return textResult("port unexposed")
}

var toolHandlers = map[string]func(json.RawMessage) *mcpToolResult{
	"instance_spawn":    handleSpawn,
	"instance_list":     handleList,
	"instance_stop":     handleStop,
	"self_info":         handleSelfInfo,
	"expose_port":       handleExposePort,
	"unexpose_port":     handleUnexposePort,
	"keepalive_acquire": handleKeepaliveAcquire,
	"keepalive_release": handleKeepaliveRelease,
}

// --- HTTP helpers ---

func doRequest(method, path string, body interface{}) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case json.RawMessage:
			bodyReader = strings.NewReader(string(v))
		case []byte:
			bodyReader = strings.NewReader(string(v))
		default:
			b, err := json.Marshal(body)
			if err != nil {
				return 0, nil, err
			}
			bodyReader = strings.NewReader(string(b))
		}
	}
	req, err := http.NewRequest(method, guestAPIBase+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("guest API unavailable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// --- Result helpers ---

func textResult(text string) *mcpToolResult {
	return &mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}}
}

func errorResult(msg string) *mcpToolResult {
	return &mcpToolResult{Content: []mcpContent{{Type: "text", Text: msg}}, IsError: true}
}

func rawJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}

// --- Main loop (stdio JSON-RPC) ---

func main() {
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		var result interface{}

		switch req.Method {
		case "initialize":
			result = mcpInitResult{
				ProtocolVersion: "2024-11-05",
				ServerInfo:      mcpServerInfo{Name: "aegis-guest", Version: version.Version()},
				Capabilities:    mcpCapabilities{Tools: &struct{}{}},
				Instructions: `Aegis Guest Orchestration — spawn and manage child VM instances from inside this VM.

You are running inside an Aegis microVM. The Guest API lets you create child instances for separate workloads:
- Use instance_spawn to start a new VM (build projects, run servers, execute tests)
- Each child gets its own isolated environment, network, and filesystem
- Children are automatically stopped when this VM stops
- Use keepalive_acquire during long work to prevent this VM from being paused

Common pattern: receive a task → spawn a child instance for the heavy work → monitor progress → report back.

Example flow:
  1. instance_spawn with command=["npm", "run", "build"], image_ref="node:22", workspace="/path/to/project", exposes=[3000]
  2. instance_list to check child state
  3. When done, instance_stop to clean up (or let it auto-pause/stop when idle)`,
			}

		case "notifications/initialized":
			continue

		case "tools/list":
			result = mcpToolsListResult{Tools: tools}

		case "tools/call":
			var callParams mcpToolCallParams
			if err := json.Unmarshal(req.Params, &callParams); err != nil {
				result = mcpToolResult{
					Content: []mcpContent{{Type: "text", Text: "invalid call params"}},
					IsError: true,
				}
				break
			}
			handler, ok := toolHandlers[callParams.Name]
			if !ok {
				result = mcpToolResult{
					Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", callParams.Name)}},
					IsError: true,
				}
				break
			}
			result = handler(callParams.Arguments)

		default:
			continue
		}

		resp := mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}
		enc.Encode(resp)
	}
}
