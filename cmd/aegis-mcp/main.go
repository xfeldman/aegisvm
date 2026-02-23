// aegis-mcp — MCP (Model Context Protocol) server for Aegis.
// Exposes aegisd instance/secret management as MCP tools over stdio.
// Talks to aegisd via the same unix socket HTTP API used by the CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xfeldman/aegisvm/internal/kit"
	"github.com/xfeldman/aegisvm/internal/version"
)

// --- JSON-RPC types ---

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"` // null for notifications
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- MCP types ---

type mcpInitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	Instructions    string          `json:"instructions"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools *struct{} `json:"tools"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// --- aegisd HTTP client ---

func socketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "aegisd.sock")
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath(), 5*time.Second)
			},
		},
		Timeout: 2 * time.Minute, // generous timeout for exec
	}
}

var client = newHTTPClient()

func apiURL(path string) string {
	return "http://aegis" + path
}

// doRequest performs an HTTP request and returns the response body bytes.
// Returns the status code, body, and any error.
func doRequest(method, path string, body interface{}) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, apiURL(path), bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("aegisd connection failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// doStreamingRequest performs an HTTP request and returns the response for
// streaming reads. Caller must close the response body.
func doStreamingRequest(method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, apiURL(path), bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

// --- Tool definitions ---

var tools = []mcpTool{
	{
		Name:        "instance_start",
		Description: "Start a new isolated Linux microVM running a command. The VM is a fresh Alpine Linux environment — host files are NOT available unless mapped via workspace. To restart a stopped or disabled instance, pass just the name without a command. Use the 'kit' parameter with installed kits (see kit_list) to get preset command, image, and capabilities — e.g. kit='agent' for a messaging-driven LLM agent. Use instance_expose to expose ports after creation.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"command":   {"type": "array", "items": {"type": "string"}, "description": "Command to run inside the VM. Paths must be VM paths (e.g. /workspace/script.py), not host paths. Not required when using a kit (the kit provides a default command)."},
				"name":      {"type": "string", "description": "Human-friendly handle for the instance (e.g. 'web', 'api'). Use this to reference the instance in other tools."},
				"kit":       {"type": "string", "description": "Kit preset name (e.g. 'agent'). Supplies default command, image, and capabilities from the kit manifest. Use kit_list to see available kits. Explicit parameters override kit defaults."},
				"workspace": {"type": "string", "description": "Absolute host directory path to live-mount inside the VM at /workspace/. Changes on the host are immediately visible inside the VM and vice versa — no restart needed. Example: '/home/user/project' becomes /workspace/ in the VM."},
				"image":     {"type": "string", "description": "OCI image reference for the VM root filesystem (e.g. 'python:3.12-alpine', 'node:22-alpine'). Default is a minimal Alpine Linux. Kit provides a default if not specified."},
				"env":       {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variables to set inside the VM."},
				"secrets":   {"type": "array", "items": {"type": "string"}, "description": "Secret keys to inject as environment variables (must be set via secret_set first)."},
				"memory_mb": {"type": "integer", "description": "VM memory in megabytes. Default: 512."},
				"vcpus":     {"type": "integer", "description": "Number of virtual CPUs. Default: 1."},
				"capabilities": {"type": "object", "description": "Guest orchestration capabilities. When set, the VM gets a Guest API on http://127.0.0.1:7777 that allows it to spawn and manage child instances. Kit provides defaults if not specified.", "properties": {"spawn": {"type": "boolean", "description": "Allow this instance to spawn child instances via the Guest API."}, "spawn_depth": {"type": "integer", "description": "Maximum nesting depth. 1 = can spawn children, but children cannot spawn grandchildren. 2 = children can also spawn."}, "max_children": {"type": "integer", "description": "Maximum number of concurrent child instances."}, "allowed_images": {"type": "array", "items": {"type": "string"}, "description": "OCI image refs children can use. Use [\"*\"] for any image."}, "max_memory_mb": {"type": "integer", "description": "Maximum memory per child instance in MB."}, "max_vcpus": {"type": "integer", "description": "Maximum vCPUs per child instance."}, "max_expose_ports": {"type": "integer", "description": "Maximum exposed ports per child instance."}}}
			}
		}`),
	},
	{
		Name:        "instance_expose",
		Description: "Expose a guest port on the host. Can be called at any time — before, during, or after the instance starts. Returns the allocated public port. Idempotent: re-exposing the same port returns the existing mapping.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name":        {"type": "string", "description": "Instance handle or ID"},
				"port":        {"type": "integer", "description": "Guest port to expose (e.g. 80, 8080)"},
				"public_port": {"type": "integer", "description": "Specific host port to use. 0 or omitted = random."},
				"protocol":    {"type": "string", "description": "Protocol hint: 'http' (default), 'tcp', 'grpc'."}
			},
			"required": ["name", "port"]
		}`),
	},
	{
		Name:        "instance_unexpose",
		Description: "Remove a port exposure from an instance. Closes the host listener for that guest port.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name":       {"type": "string", "description": "Instance handle or ID"},
				"guest_port": {"type": "integer", "description": "Guest port to unexpose"}
			},
			"required": ["name", "guest_port"]
		}`),
	},
	{
		Name:        "instance_list",
		Description: "List VM instances. Optionally filter by state.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"state": {"type": "string", "enum": ["running", "stopped", "paused"], "description": "Filter by instance state"}
			}
		}`),
	},
	{
		Name:        "instance_info",
		Description: "Get detailed information about a specific instance.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Instance handle or ID"}
			},
			"required": ["name"]
		}`),
	},
	{
		Name:        "instance_disable",
		Description: "Disable an instance. Closes all port listeners, stops the VM, and prevents auto-wake. The instance record is preserved and can be re-enabled with instance_start.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Instance handle or ID"}
			},
			"required": ["name"]
		}`),
	},
	{
		Name:        "instance_delete",
		Description: "Delete an instance entirely, removing its record.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Instance handle or ID"}
			},
			"required": ["name"]
		}`),
	},
	{
		Name:        "exec",
		Description: "Execute a command inside a running VM instance and return its full output. The command runs in the VM filesystem — use /workspace/ paths to access workspace-mounted files.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name":    {"type": "string", "description": "Instance handle or ID"},
				"command": {"type": "array", "items": {"type": "string"}, "description": "Command to execute inside the VM (e.g. [\"ls\", \"/workspace/\"])"}
			},
			"required": ["name", "command"]
		}`),
	},
	{
		Name:        "logs",
		Description: "Get recent logs from an instance (not streaming).",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Instance handle or ID"},
				"tail": {"type": "integer", "description": "Number of lines to return (default 50)"}
			},
			"required": ["name"]
		}`),
	},
	{
		Name:        "secret_set",
		Description: "Set a secret key-value pair. Secrets can be injected into instances.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"key":   {"type": "string", "description": "Secret name"},
				"value": {"type": "string", "description": "Secret value"}
			},
			"required": ["key", "value"]
		}`),
	},
	{
		Name:        "secret_list",
		Description: "List all secret names (values are not returned).",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {}
		}`),
	},
	{
		Name:        "secret_delete",
		Description: "Delete a secret by name.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"key": {"type": "string", "description": "Secret name to delete"}
			},
			"required": ["key"]
		}`),
	},
	{
		Name:        "kit_list",
		Description: "List installed kits. Kits are optional add-on bundles that provide preset configurations for instances (command, image, capabilities). Use the kit name with instance_start's 'kit' parameter.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {}
		}`),
	},
	{
		Name:        "tether_send",
		Description: "Send a natural language message to the LLM agent running inside a VM instance. The instance wakes automatically if paused or stopped (wake-on-message). The agent processes the message using its own LLM context and tools, then streams a response. Use tether_read to receive the response. Use this for delegation ('go research X'), debugging ('what are you working on'), or any task that benefits from an isolated agent with its own context and tools.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"instance":   {"type": "string", "description": "Instance handle or ID"},
				"text":       {"type": "string", "description": "Message text to send to the agent"},
				"session_id": {"type": "string", "description": "Session identifier for conversation threading. Use a stable ID per conversation (e.g. 'debug-1', 'task-analyze'). Default: 'default'."}
			},
			"required": ["instance", "text"]
		}`),
	},
	{
		Name:        "tether_read",
		Description: "Read response frames from the LLM agent inside a VM instance. Returns new frames since the given cursor. Supports long-polling: set wait_ms (e.g. 15000) to block until frames arrive or timeout. Call in a loop until you see an 'assistant.done' frame, which contains the complete response. Use ingress_seq from tether_send as your initial after_seq, then use next_seq from each response as the cursor for the next call.",
		InputSchema: rawJSON(`{
			"type": "object",
			"properties": {
				"instance":   {"type": "string", "description": "Instance handle or ID"},
				"session_id": {"type": "string", "description": "Session ID (must match tether_send). Default: 'default'."},
				"after_seq":  {"type": "integer", "description": "Return frames with seq > this value. Start with 0 or use ingress_seq from tether_send."},
				"limit":      {"type": "integer", "description": "Max frames to return. Default: 50, max: 200."},
				"wait_ms":    {"type": "integer", "description": "Long-poll timeout in milliseconds. Block until frames arrive or timeout. 0 = return immediately. Default: 0, max: 30000."},
				"types":      {"type": "array", "items": {"type": "string"}, "description": "Filter by frame types. Example: ['assistant.delta', 'assistant.done']. Default: all types."}
			},
			"required": ["instance"]
		}`),
	},
}

func rawJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}

// --- Tool handlers ---

func handleInstanceStart(args json.RawMessage) *mcpToolResult {
	var params struct {
		Command      []string               `json:"command"`
		Name         string                 `json:"name"`
		Kit          string                 `json:"kit"`
		Workspace    string                 `json:"workspace"`
		Image        string                 `json:"image"`
		Env          map[string]string      `json:"env"`
		Secrets      []string               `json:"secrets"`
		MemoryMB     int                    `json:"memory_mb"`
		VCPUs        int                    `json:"vcpus"`
		Capabilities map[string]interface{} `json:"capabilities"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}

	// If no command, no kit, but name is given, this is a restart of a stopped instance
	if len(params.Command) == 0 && params.Kit == "" && params.Name != "" {
		body := map[string]interface{}{"handle": params.Name}
		status, data, err := doRequest("POST", "/v1/instances", body)
		if err != nil {
			return errorResult(err.Error())
		}
		if status >= 400 {
			return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
		}
		return textResult(string(data))
	}

	// Apply kit defaults if specified
	if params.Kit != "" {
		manifest, err := loadKitManifest(params.Kit)
		if err != nil {
			return errorResult(fmt.Sprintf("kit %q: %v", params.Kit, err))
		}
		if params.Image == "" {
			if base, ok := manifest["image"].(map[string]interface{}); ok {
				if b, ok := base["base"].(string); ok {
					params.Image = b
				}
			}
		}
		if len(params.Command) == 0 {
			if defaults, ok := manifest["defaults"].(map[string]interface{}); ok {
				if cmd, ok := defaults["command"].([]interface{}); ok {
					for _, c := range cmd {
						if s, ok := c.(string); ok {
							params.Command = append(params.Command, s)
						}
					}
				}
			}
		}
		if len(params.Capabilities) == 0 {
			if defaults, ok := manifest["defaults"].(map[string]interface{}); ok {
				if caps, ok := defaults["capabilities"].(map[string]interface{}); ok {
					params.Capabilities = caps
				}
			}
		}
	}

	if len(params.Command) == 0 {
		return errorResult("command is required (or use a kit that provides a default command)")
	}

	body := map[string]interface{}{
		"command": params.Command,
	}
	if params.Kit != "" {
		body["kit"] = params.Kit
	}
	if params.Name != "" {
		body["handle"] = params.Name
	}
	if params.Workspace != "" {
		body["workspace"] = params.Workspace
	}
	if params.Image != "" {
		body["image_ref"] = params.Image
	}
	if len(params.Env) > 0 {
		body["env"] = params.Env
	}
	if len(params.Secrets) > 0 {
		body["secrets"] = params.Secrets
	}
	if params.MemoryMB > 0 {
		body["memory_mb"] = params.MemoryMB
	}
	if params.VCPUs > 0 {
		body["vcpus"] = params.VCPUs
	}
	if len(params.Capabilities) > 0 {
		body["capabilities"] = params.Capabilities
	}

	status, data, err := doRequest("POST", "/v1/instances", body)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleInstanceList(args json.RawMessage) *mcpToolResult {
	var params struct {
		State string `json:"state"`
	}
	if args != nil {
		json.Unmarshal(args, &params)
	}

	path := "/v1/instances"
	if params.State != "" {
		path += "?state=" + params.State
	}

	status, data, err := doRequest("GET", path, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleInstanceInfo(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Name == "" {
		return errorResult("name is required")
	}

	status, data, err := doRequest("GET", "/v1/instances/"+params.Name, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleInstanceDisable(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Name == "" {
		return errorResult("name is required")
	}

	status, data, err := doRequest("POST", "/v1/instances/"+params.Name+"/disable", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult("instance disabled")
}

func handleInstanceDelete(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Name == "" {
		return errorResult("name is required")
	}

	status, data, err := doRequest("DELETE", "/v1/instances/"+params.Name, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult("instance deleted")
}

func handleExec(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name    string   `json:"name"`
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Name == "" || len(params.Command) == 0 {
		return errorResult("name and command are required")
	}

	body := map[string]interface{}{
		"command": params.Command,
	}

	resp, err := doStreamingRequest("POST", "/v1/instances/"+params.Name+"/exec", body)
	if err != nil {
		return errorResult(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return errorResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(data)))
	}

	// Read NDJSON stream: collect output lines, wait for done marker
	var lines []string
	exitCode := -1
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var entry map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		// Done marker
		if done, ok := entry["done"].(bool); ok && done {
			if code, ok := entry["exit_code"].(float64); ok {
				exitCode = int(code)
			}
			break
		}
		// Log line
		if line, ok := entry["line"].(string); ok {
			lines = append(lines, line)
		}
	}

	output := strings.Join(lines, "\n")
	if exitCode != 0 {
		output += fmt.Sprintf("\n[exit code: %d]", exitCode)
	}
	if output == "" {
		output = fmt.Sprintf("[no output, exit code: %d]", exitCode)
	}
	return textResult(output)
}

func handleLogs(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name string `json:"name"`
		Tail int    `json:"tail"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Name == "" {
		return errorResult("name is required")
	}
	if params.Tail <= 0 {
		params.Tail = 50
	}

	path := fmt.Sprintf("/v1/instances/%s/logs?tail=%d", params.Name, params.Tail)
	resp, err := doStreamingRequest("GET", path, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return errorResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(data)))
	}

	// Read NDJSON log entries
	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var entry struct {
			Stream string `json:"stream"`
			Line   string `json:"line"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		prefix := ""
		if entry.Stream == "stderr" {
			prefix = "[stderr] "
		}
		lines = append(lines, prefix+entry.Line)
	}

	if len(lines) == 0 {
		return textResult("[no logs]")
	}
	return textResult(strings.Join(lines, "\n"))
}

func handleSecretSet(args json.RawMessage) *mcpToolResult {
	var params struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Key == "" || params.Value == "" {
		return errorResult("key and value are required")
	}

	body := map[string]string{"value": params.Value}
	status, data, err := doRequest("PUT", "/v1/secrets/"+params.Key, body)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(fmt.Sprintf("secret %q set", params.Key))
}

func handleSecretList(args json.RawMessage) *mcpToolResult {
	status, data, err := doRequest("GET", "/v1/secrets", nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleSecretDelete(args json.RawMessage) *mcpToolResult {
	var params struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Key == "" {
		return errorResult("key is required")
	}

	status, data, err := doRequest("DELETE", "/v1/secrets/"+params.Key, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(fmt.Sprintf("secret %q deleted", params.Key))
}

func handleInstanceExpose(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name       string `json:"name"`
		Port       int    `json:"port"`
		PublicPort int    `json:"public_port"`
		Protocol   string `json:"protocol"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Name == "" || params.Port <= 0 {
		return errorResult("name and port are required")
	}

	body := map[string]interface{}{
		"port": params.Port,
	}
	if params.PublicPort > 0 {
		body["public_port"] = params.PublicPort
	}
	if params.Protocol != "" {
		body["protocol"] = params.Protocol
	}

	status, data, err := doRequest("POST", "/v1/instances/"+params.Name+"/expose", body)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult(string(data))
}

func handleInstanceUnexpose(args json.RawMessage) *mcpToolResult {
	var params struct {
		Name      string `json:"name"`
		GuestPort int    `json:"guest_port"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Name == "" || params.GuestPort <= 0 {
		return errorResult("name and guest_port are required")
	}

	status, data, err := doRequest("DELETE", fmt.Sprintf("/v1/instances/%s/expose/%d", params.Name, params.GuestPort), nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}
	return textResult("port unexposed")
}

func loadKitManifest(name string) (map[string]interface{}, error) {
	manifest, err := kit.LoadManifest(name)
	if err != nil {
		return nil, err
	}
	// Convert to generic map for JSON flexibility in handleInstanceStart
	data, _ := json.Marshal(manifest)
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	return m, nil
}

func handleKitList(args json.RawMessage) *mcpToolResult {
	manifests, _ := kit.ListManifests()
	if len(manifests) == 0 {
		return textResult("No kits installed.")
	}

	data, _ := json.MarshalIndent(manifests, "", "  ")
	return textResult(string(data))
}

func handleTetherSend(args json.RawMessage) *mcpToolResult {
	var params struct {
		Instance  string `json:"instance"`
		Text      string `json:"text"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Instance == "" || params.Text == "" {
		return errorResult("instance and text are required")
	}
	if params.SessionID == "" {
		params.SessionID = "default"
	}

	msgID := fmt.Sprintf("host-%d", time.Now().UnixNano())
	frame := map[string]interface{}{
		"v":    1,
		"type": "user.message",
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"session": map[string]string{
			"channel": "host",
			"id":      params.SessionID,
		},
		"msg_id":  msgID,
		"payload": map[string]string{"text": params.Text},
	}

	status, data, err := doRequest("POST", "/v1/instances/"+params.Instance+"/tether", frame)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}

	// Parse ingress_seq from response
	var resp struct {
		IngressSeq int64 `json:"ingress_seq"`
	}
	json.Unmarshal(data, &resp)

	result := map[string]interface{}{
		"msg_id":      msgID,
		"session_id":  params.SessionID,
		"ingress_seq": resp.IngressSeq,
	}
	resultJSON, _ := json.Marshal(result)
	return textResult(string(resultJSON))
}

func handleTetherRead(args json.RawMessage) *mcpToolResult {
	var params struct {
		Instance  string   `json:"instance"`
		SessionID string   `json:"session_id"`
		AfterSeq  int64    `json:"after_seq"`
		Limit     int      `json:"limit"`
		WaitMs    int      `json:"wait_ms"`
		Types     []string `json:"types"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("invalid arguments: " + err.Error())
	}
	if params.Instance == "" {
		return errorResult("instance is required")
	}
	if params.SessionID == "" {
		params.SessionID = "default"
	}
	if params.Limit <= 0 {
		params.Limit = 50
	}
	if params.WaitMs > 30000 {
		params.WaitMs = 30000
	}

	// Build poll URL
	path := fmt.Sprintf("/v1/instances/%s/tether/poll?channel=host&session_id=%s&after_seq=%d&limit=%d&wait_ms=%d",
		params.Instance, params.SessionID, params.AfterSeq, params.Limit, params.WaitMs)
	if len(params.Types) > 0 {
		path += "&types=" + strings.Join(params.Types, ",")
	}

	status, data, err := doRequest("GET", path, nil)
	if err != nil {
		return errorResult(err.Error())
	}
	if status >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", status, string(data)))
	}

	return textResult(string(data))
}

// --- Result helpers ---

func textResult(text string) *mcpToolResult {
	return &mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: text}},
	}
}

func errorResult(msg string) *mcpToolResult {
	return &mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// --- Tool dispatch ---

var toolHandlers = map[string]func(json.RawMessage) *mcpToolResult{
	"instance_start":    handleInstanceStart,
	"instance_list":     handleInstanceList,
	"instance_info":     handleInstanceInfo,
	"instance_expose":   handleInstanceExpose,
	"instance_unexpose": handleInstanceUnexpose,
	"instance_disable":  handleInstanceDisable,
	"instance_delete":   handleInstanceDelete,
	"exec":              handleExec,
	"logs":              handleLogs,
	"secret_set":        handleSecretSet,
	"secret_list":       handleSecretList,
	"secret_delete":     handleSecretDelete,
	"kit_list":          handleKitList,
	"tether_send":       handleTetherSend,
	"tether_read":       handleTetherRead,
}

// --- Main loop ---

func main() {
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	// Allow large messages (16MB)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// Write parse error only if we can extract an ID
			enc.Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error"},
				ID:      nil,
			})
			continue
		}

		// Notifications (no id) — just acknowledge silently
		if req.ID == nil {
			continue
		}

		var result interface{}
		var rpcErr *rpcError

		switch req.Method {
		case "initialize":
			result = mcpInitializeResult{
				ProtocolVersion: "2024-11-05",
				ServerInfo: mcpServerInfo{
					Name:    "aegisvm",
					Version: version.Version(),
				},
				Capabilities: mcpCapabilities{
					Tools: &struct{}{},
				},
				Instructions: `AegisVM runs commands inside isolated Linux microVMs on the local machine.

Key concepts:
- Each instance is a lightweight VM running a single command. The VM runs Alpine Linux (ARM64).
- Commands run INSIDE the VM, not on the host. Host files are NOT available inside the VM unless you use a workspace.
- Workspace: pass an absolute host directory path as the "workspace" parameter. It will be mounted at /workspace/ inside the VM. Example: '/home/user/project' becomes /workspace/ in the VM.
- Ports: the VM has its own network. To access a server running inside, use instance_expose to map guest ports to the host. Ports can be exposed/unexposed at any time — before, during, or after the instance starts.
- The base VM has basic tools (sh, ls, cat, etc). For Python, Node, or other runtimes, use the "image" parameter with an OCI image ref (e.g. "python:3.12", "node:20").
- Use "exec" to run commands inside a running instance. Use "logs" to see instance output.
- Use "name" to give instances human-friendly handles for easy reference.

Kits:
- Kits are optional add-on bundles that provide preset configurations for instances.
- Use kit_list to see installed kits. Each kit provides a default command, image, and capabilities.
- Use instance_start with kit="<name>" to create an instance with kit defaults. Explicit parameters override.
- Example: instance_start with kit="agent", name="my-agent", secrets=["OPENAI_API_KEY"] creates a messaging-driven LLM agent.

Tether — talk to an agent inside a VM:
- Kit instances (--kit agent) run an LLM agent inside the VM that you can communicate with via tether.
- tether_send sends a natural language message to the in-VM agent. The VM wakes automatically if paused or stopped.
- tether_read reads the agent's response. It supports long-polling (wait_ms) so you can block until the response arrives.
- The agent inside the VM has its own LLM context, session history, and access to MCP tools (can spawn child VMs, run commands, manage files).
- Use tether for delegation ("research X and report back"), debugging ("what is your current state"), or orchestration.

When to use tether vs exec:
- exec runs a shell command inside the VM and returns output. Good for one-off commands (ls, cat, pip install).
- tether sends a message to the in-VM LLM agent and gets an intelligent response. Good for tasks that need reasoning, multi-step work, or conversation context.

Tether conversation pattern:
  1. tether_send(instance="my-agent", text="Analyze the CSV file in /workspace/data.csv")
     Returns {msg_id, session_id, ingress_seq}
  2. tether_read(instance="my-agent", after_seq=<ingress_seq>, wait_ms=15000)
     Returns {frames: [...], next_seq, timed_out}
     Look for type="assistant.done" in frames — that's the complete response.
  3. If no assistant.done yet, call tether_read again with after_seq=<next_seq> to continue reading.
  4. Frame types: "status.presence" (agent is thinking), "assistant.delta" (streaming text chunk), "assistant.done" (complete response).

Sessions: each tether_send uses a session_id (default: "default"). Use different session_ids for independent conversations with the same agent. Sessions maintain separate conversation histories.

Example — delegate research to an agent:
  1. instance_start with kit="agent", name="researcher", secrets=["OPENAI_API_KEY"]
  2. tether_send(instance="researcher", text="Find the top 5 Python libraries for data visualization and explain each")
  3. tether_read(instance="researcher", after_seq=<ingress_seq>, wait_ms=15000) — repeat until assistant.done

Example — run a script directly:
  1. instance_start with workspace="/path/to/project", command=["python3", "/workspace/script.py"], name="myapp"
  2. exec with name="myapp", command=["ls", "/workspace/"] to see mounted files
  3. logs with name="myapp" to check output`,
			}

		case "tools/list":
			result = mcpToolsListResult{Tools: tools}

		case "tools/call":
			var callParams mcpToolCallParams
			if err := json.Unmarshal(req.Params, &callParams); err != nil {
				rpcErr = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			} else {
				handler, ok := toolHandlers[callParams.Name]
				if !ok {
					rpcErr = &rpcError{Code: -32602, Message: "unknown tool: " + callParams.Name}
				} else {
					result = handler(callParams.Arguments)
				}
			}

		default:
			rpcErr = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}

		enc.Encode(jsonRPCResponse{
			JSONRPC: "2.0",
			Result:  result,
			Error:   rpcErr,
			ID:      req.ID,
		})
	}
}
