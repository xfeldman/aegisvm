package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const mcpConfigPath = "/workspace/.aegis/mcp.json"

// MCPConfig is the workspace MCP server configuration.
type MCPConfig struct {
	Servers map[string]MCPServerConfig `json:"servers"`
}

// MCPServerConfig describes a single MCP server to spawn.
type MCPServerConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// MCPClient communicates with an MCP server over stdio JSON-RPC.
type MCPClient struct {
	name   string // server name from config (used as tool prefix)
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID int
	tools  []Tool            // tools with namespaced names
	names  map[string]string // namespaced name → original MCP name
}

// initMCPTools discovers and starts MCP servers, assembles the full tool list.
func (a *Agent) initMCPTools() {
	a.allTools = append(a.allTools, builtinTools...)
	a.mcpClients = make(map[string]*MCPClient)

	config := loadMCPConfig()

	for name, serverCfg := range config.Servers {
		client, err := newMCPClient(name, serverCfg.Command, serverCfg.Args)
		if err != nil {
			log.Printf("MCP [%s]: failed to start: %v", name, err)
			continue
		}
		a.mcpClients[name] = client
		a.allTools = append(a.allTools, client.tools...)
		log.Printf("MCP [%s]: loaded %d tools", name, len(client.tools))
	}
}

// loadMCPConfig reads /workspace/.aegis/mcp.json.
// If no config exists, auto-discovers aegis-mcp-guest from the rootfs.
func loadMCPConfig() MCPConfig {
	config := MCPConfig{Servers: make(map[string]MCPServerConfig)}

	data, err := os.ReadFile(mcpConfigPath)
	if err == nil {
		if json.Unmarshal(data, &config) == nil && len(config.Servers) > 0 {
			log.Printf("MCP: loaded config from %s (%d servers)", mcpConfigPath, len(config.Servers))
			return config
		}
	}

	// No config — auto-discover aegis-mcp-guest
	mcpGuestBin := "/usr/bin/aegis-mcp-guest"
	if _, err := os.Stat(mcpGuestBin); err == nil {
		config.Servers["aegis"] = MCPServerConfig{Command: mcpGuestBin}
		log.Printf("MCP: auto-discovered aegis-mcp-guest")
	} else {
		log.Printf("MCP: no config and no aegis-mcp-guest found")
	}

	return config
}

func (a *Agent) closeMCP() {
	for _, mc := range a.mcpClients {
		mc.close()
	}
}

func newMCPClient(name, command string, args []string) (*MCPClient, error) {
	// Resolve command in PATH if not absolute
	binPath := command
	if !filepath.IsAbs(command) {
		if p, err := exec.LookPath(command); err == nil {
			binPath = p
		}
	}

	cmd := exec.Command(binPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &MCPClient{
		name:   name,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		nextID: 1,
		names:  make(map[string]string),
	}
	c.stdout.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Initialize MCP handshake
	_, err = c.call("initialize", map[string]interface{}{
		"capabilities": map[string]interface{}{},
	})
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// Discover tools
	toolsResp, err := c.call("tools/list", nil)
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("tools/list: %w", err)
	}

	var toolsList struct {
		Tools []struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			InputSchema interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	json.Unmarshal(toolsResp, &toolsList)

	for _, t := range toolsList.Tools {
		nsName := name + "_" + t.Name
		c.names[nsName] = t.Name
		c.tools = append(c.tools, Tool{
			Name:        nsName,
			Description: fmt.Sprintf("[%s] %s", name, t.Description),
			InputSchema: t.InputSchema,
		})
	}

	return c, nil
}

func (c *MCPClient) call(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		req["params"] = params
	}

	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	if !c.stdout.Scan() {
		return nil, fmt.Errorf("no response from MCP server")
	}

	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(c.stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// HasTool returns true if this client owns the namespaced tool name.
func (c *MCPClient) HasTool(nsName string) bool {
	_, ok := c.names[nsName]
	return ok
}

// CallTool invokes an MCP tool by its namespaced name.
func (c *MCPClient) CallTool(nsName string, input json.RawMessage) (string, error) {
	mcpName, ok := c.names[nsName]
	if !ok {
		return "", fmt.Errorf("tool %s not found in MCP server %s", nsName, c.name)
	}

	var inputMap interface{}
	json.Unmarshal(input, &inputMap)

	result, err := c.call("tools/call", map[string]interface{}{
		"name":      mcpName,
		"arguments": inputMap,
	})
	if err != nil {
		return "", err
	}

	var toolResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(result, &toolResult)

	var texts []string
	for _, c := range toolResult.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func (c *MCPClient) close() {
	c.stdin.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
}
