package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// builtinTools are workspace-scoped tools available to every agent.
var builtinTools = []Tool{
	{
		Name:        "bash",
		Description: "Execute a shell command. Working directory is /workspace/. Returns stdout, stderr, and exit code.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]string{"type": "string", "description": "The shell command to execute"},
			},
			"required": []string{"command"},
		},
	},
	{
		Name:        "read_file",
		Description: "Read the contents of a file. Path must be under /workspace/.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]string{"type": "string", "description": "File path (relative to /workspace/ or absolute under /workspace/)"},
			},
			"required": []string{"path"},
		},
	},
	{
		Name:        "write_file",
		Description: "Write content to a file. Path must be under /workspace/. Creates parent directories if needed.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]string{"type": "string", "description": "File path"},
				"content": map[string]string{"type": "string", "description": "File content to write"},
			},
			"required": []string{"path", "content"},
		},
	},
	{
		Name:        "list_files",
		Description: "List files and directories. Path must be under /workspace/.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]string{"type": "string", "description": "Directory path (defaults to /workspace/)"},
			},
		},
	},
}

// executeTool dispatches a tool call to the appropriate handler.
func (a *Agent) executeTool(name string, input json.RawMessage) string {
	switch name {
	case "bash":
		return toolBash(input)
	case "read_file":
		return toolReadFile(input)
	case "write_file":
		return toolWriteFile(input)
	case "list_files":
		return toolListFiles(input)
	default:
		// Try MCP tools
		for _, mc := range a.mcpClients {
			if mc.HasTool(name) {
				result, err := mc.CallTool(name, input)
				if err != nil {
					return fmt.Sprintf("error: %v", err)
				}
				return result
			}
		}
		return fmt.Sprintf("unknown tool: %s", name)
	}
}

func toolBash(input json.RawMessage) string {
	var params struct {
		Command string `json:"command"`
	}
	json.Unmarshal(input, &params)
	if params.Command == "" {
		return "error: command is required"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", params.Command)
	cmd.Dir = workspaceRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	var result strings.Builder
	if stdout.Len() > 0 {
		result.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("stderr: ")
		result.WriteString(stderr.String())
	}
	if err != nil {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString(fmt.Sprintf("exit: %v", err))
	}
	s := result.String()
	if len(s) > 10000 {
		s = s[:10000] + "\n... (truncated)"
	}
	return s
}

func resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceRoot, path)
	}
	path = filepath.Clean(path)
	if !strings.HasPrefix(path, workspaceRoot) {
		return "", fmt.Errorf("path must be under %s", workspaceRoot)
	}
	return path, nil
}

func toolReadFile(input json.RawMessage) string {
	var params struct{ Path string `json:"path"` }
	json.Unmarshal(input, &params)
	path, err := resolvePath(params.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	s := string(data)
	if len(s) > 50000 {
		s = s[:50000] + "\n... (truncated)"
	}
	return s
}

func toolWriteFile(input json.RawMessage) string {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	json.Unmarshal(input, &params)
	path, err := resolvePath(params.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(params.Content), 0644); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(params.Content), params.Path)
}

func toolListFiles(input json.RawMessage) string {
	var params struct{ Path string `json:"path"` }
	json.Unmarshal(input, &params)
	if params.Path == "" {
		params.Path = workspaceRoot
	}
	path, err := resolvePath(params.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	var lines []string
	for _, e := range entries {
		info, _ := e.Info()
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		lines = append(lines, fmt.Sprintf("%s%s  %d bytes", e.Name(), suffix, size))
	}
	if len(lines) == 0 {
		return "(empty directory)"
	}
	return strings.Join(lines, "\n")
}
