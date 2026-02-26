package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		Description: "Read the contents of a file. Path must be under /workspace/. Supports partial reads with start_line/end_line.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]string{"type": "string", "description": "File path (relative to /workspace/ or absolute under /workspace/)"},
				"start_line": map[string]string{"type": "integer", "description": "Start line (1-indexed). If set, returns numbered lines."},
				"end_line":   map[string]string{"type": "integer", "description": "End line (1-indexed, inclusive). Defaults to end of file."},
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
		Name:        "edit_file",
		Description: "Edit a file by replacing text or a line range. Path must be under /workspace/. Use old_text/new_text for text replacement, or start_line/end_line for line range replacement. Returns a diff of the changes.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]string{"type": "string", "description": "File path (relative to /workspace/ or absolute under /workspace/)"},
				"old_text":   map[string]string{"type": "string", "description": "Text to find and replace. Must match exactly (including whitespace/indentation)."},
				"new_text":   map[string]string{"type": "string", "description": "Replacement text."},
				"start_line": map[string]string{"type": "integer", "description": "Start line (1-indexed) for line range replacement."},
				"end_line":   map[string]string{"type": "integer", "description": "End line (1-indexed, inclusive) for line range replacement."},
				"occurrence": map[string]string{"type": "integer", "description": "Which occurrence to replace (1-indexed). Default: replace if unique, error if ambiguous."},
			},
			"required": []string{"path", "new_text"},
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
	{
		Name:        "glob",
		Description: "Find files matching a glob pattern. Supports ** for recursive matching (e.g. '**/*.go', 'src/**/*.ts'). Path must be under /workspace/. Returns up to 200 results.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]string{"type": "string", "description": "Glob pattern (e.g. '**/*.go', '*.py', 'src/**/*.ts')"},
				"path":    map[string]string{"type": "string", "description": "Base directory (defaults to /workspace/)"},
			},
			"required": []string{"pattern"},
		},
	},
	{
		Name:        "grep",
		Description: "Search file contents with a regex pattern. Path must be under /workspace/. Returns up to 50 matches with file, line number, and text.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]string{"type": "string", "description": "Regular expression pattern to search for"},
				"path":    map[string]string{"type": "string", "description": "File or directory to search (defaults to /workspace/)"},
				"include": map[string]string{"type": "string", "description": "Glob filter for filenames (e.g. '*.go', '*.py')"},
			},
			"required": []string{"pattern"},
		},
	},
	{
		Name:        "memory_store",
		Description: "Store a fact or note in persistent memory. Memories are automatically surfaced in future conversations when relevant. Do NOT store secrets, tokens, or transient task context.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text":  map[string]string{"type": "string", "description": "The fact or note to remember (max 500 chars)"},
				"tags":  map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Optional classification tags (0-5)"},
				"scope": map[string]string{"type": "string", "description": "Scope: 'user', 'workspace', or 'session' (default 'workspace')"},
			},
			"required": []string{"text"},
		},
	},
	{
		Name:        "memory_search",
		Description: "Search stored memories by keyword and/or tag. Returns up to 20 matches, newest first.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]string{"type": "string", "description": "Keyword search across memory text (case-insensitive)"},
				"tag":   map[string]string{"type": "string", "description": "Filter by tag"},
			},
		},
	},
	{
		Name:        "memory_delete",
		Description: "Delete a memory by its ID (e.g. 'm-1'). Use this to remove outdated or incorrect memories.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]string{"type": "string", "description": "Memory ID to delete (e.g. 'm-1')"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "cron_create",
		Description: "Create a scheduled task that runs on a cron schedule. The task message is sent as a user message to a dedicated session. Schedule is evaluated in host local time.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"schedule":    map[string]string{"type": "string", "description": "Cron expression, 5 fields: minute hour day-of-month month day-of-week (e.g. '*/5 * * * *', '0 9 * * 1-5')"},
				"message":     map[string]string{"type": "string", "description": "Task description sent as a user message when the cron fires (max 1000 chars)"},
				"session":     map[string]string{"type": "string", "description": "Session ID for the cron's conversation (default: 'cron-{id}')"},
				"on_conflict": map[string]string{"type": "string", "description": "'skip' (default) — drop fire if previous run active. 'queue' — send anyway."},
			},
			"required": []string{"schedule", "message"},
		},
	},
	{
		Name:        "cron_list",
		Description: "List all scheduled cron tasks.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "cron_delete",
		Description: "Delete a scheduled cron task by ID.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]string{"type": "string", "description": "Cron entry ID (e.g. 'cron-0')"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "cron_enable",
		Description: "Enable a paused cron task.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]string{"type": "string", "description": "Cron entry ID"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "cron_disable",
		Description: "Pause a cron task without deleting it.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]string{"type": "string", "description": "Cron entry ID"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "web_fetch",
		Description: "Fetch a URL and return its text content. HTML is stripped to readable text. Returns up to 10KB of text.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]string{"type": "string", "description": "URL to fetch (must start with http:// or https://)"},
			},
			"required": []string{"url"},
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
	case "edit_file":
		return toolEditFile(input)
	case "list_files":
		return toolListFiles(input)
	case "glob":
		return toolGlob(input)
	case "grep":
		return toolGrep(input)
	case "memory_store":
		return a.toolMemoryStore(input)
	case "memory_search":
		return a.toolMemorySearch(input)
	case "memory_delete":
		return a.toolMemoryDelete(input)
	case "cron_create":
		return a.toolCronCreate(input)
	case "cron_list":
		return a.toolCronList(input)
	case "cron_delete":
		return a.toolCronDelete(input)
	case "cron_enable":
		return a.toolCronEnable(input)
	case "cron_disable":
		return a.toolCronDisable(input)
	case "web_fetch":
		return toolWebFetch(input)
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
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
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

	if params.StartLine > 0 {
		lines := strings.Split(s, "\n")
		start := params.StartLine - 1 // 0-indexed
		end := params.EndLine
		if end == 0 || end > len(lines) {
			end = len(lines)
		}
		if start < 0 {
			start = 0
		}
		if start >= len(lines) {
			return fmt.Sprintf("error: start_line %d beyond end of file (%d lines)", params.StartLine, len(lines))
		}

		var buf strings.Builder
		fmt.Fprintf(&buf, "(%d lines total, showing %d-%d)\n", len(lines), start+1, end)
		for i := start; i < end; i++ {
			fmt.Fprintf(&buf, "%4d | %s\n", i+1, lines[i])
		}
		return buf.String()
	}

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

func toolEditFile(input json.RawMessage) string {
	var params struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		StartLine  int    `json:"start_line"`
		EndLine    int    `json:"end_line"`
		Occurrence int    `json:"occurrence"`
	}
	json.Unmarshal(input, &params)

	path, err := resolvePath(params.Path)
	if err != nil {
		return jsonError(err.Error())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return jsonError(err.Error())
	}

	original := string(data)
	var result string

	if params.OldText != "" {
		// Text match mode
		count := strings.Count(original, params.OldText)
		if count == 0 {
			return jsonError("old_text not found in file")
		}
		if count > 1 && params.Occurrence == 0 {
			return jsonError(fmt.Sprintf("old_text found %d times — specify occurrence (1-indexed)", count))
		}

		if params.Occurrence > 0 {
			if params.Occurrence > count {
				return jsonError(fmt.Sprintf("occurrence %d requested but only %d found", params.Occurrence, count))
			}
			result = replaceNth(original, params.OldText, params.NewText, params.Occurrence)
		} else {
			result = strings.Replace(original, params.OldText, params.NewText, 1)
		}
	} else if params.StartLine > 0 {
		// Line range mode
		lines := strings.Split(original, "\n")
		start := params.StartLine - 1 // 0-indexed
		end := params.EndLine
		if end == 0 {
			end = params.StartLine
		}
		if start < 0 || start >= len(lines) || end > len(lines) || start > end {
			return jsonError(fmt.Sprintf("invalid line range %d-%d (file has %d lines)", params.StartLine, end, len(lines)))
		}

		newLines := strings.Split(params.NewText, "\n")
		resultLines := make([]string, 0, len(lines)-end+start+len(newLines))
		resultLines = append(resultLines, lines[:start]...)
		resultLines = append(resultLines, newLines...)
		resultLines = append(resultLines, lines[end:]...)
		result = strings.Join(resultLines, "\n")
	} else {
		return jsonError("old_text or start_line is required")
	}

	if err := os.WriteFile(path, []byte(result), 0644); err != nil {
		return jsonError(err.Error())
	}

	diff := computeDiff(original, result)
	return jsonResult(map[string]interface{}{
		"ok":   true,
		"path": params.Path,
		"diff": diff,
	})
}

// replaceNth replaces the nth occurrence (1-indexed) of old with new in s.
func replaceNth(s, old, new string, n int) string {
	idx := 0
	for i := 0; i < n; i++ {
		pos := strings.Index(s[idx:], old)
		if pos < 0 {
			return s
		}
		idx += pos
		if i < n-1 {
			idx += len(old)
		}
	}
	return s[:idx] + new + s[idx+len(old):]
}

// computeDiff produces a simple unified-style diff between old and new text.
func computeDiff(oldText, newText string) string {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	// Find common prefix
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}

	// Find common suffix
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	var diff strings.Builder
	// Context before
	ctxStart := prefix - 3
	if ctxStart < 0 {
		ctxStart = 0
	}
	for i := ctxStart; i < prefix; i++ {
		fmt.Fprintf(&diff, " %s\n", oldLines[i])
	}
	// Removed lines
	for i := prefix; i < len(oldLines)-suffix; i++ {
		fmt.Fprintf(&diff, "-%s\n", oldLines[i])
	}
	// Added lines
	for i := prefix; i < len(newLines)-suffix; i++ {
		fmt.Fprintf(&diff, "+%s\n", newLines[i])
	}
	// Context after
	ctxEnd := len(oldLines) - suffix + 3
	if ctxEnd > len(oldLines) {
		ctxEnd = len(oldLines)
	}
	for i := len(oldLines) - suffix; i < ctxEnd; i++ {
		fmt.Fprintf(&diff, " %s\n", oldLines[i])
	}

	s := diff.String()
	if len(s) > 2000 {
		s = s[:2000] + "\n... (truncated)"
	}
	return s
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
		sfx := ""
		if e.IsDir() {
			sfx = "/"
		}
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		lines = append(lines, fmt.Sprintf("%s%s  %d bytes", e.Name(), sfx, size))
	}
	if len(lines) == 0 {
		return "(empty directory)"
	}
	return strings.Join(lines, "\n")
}

func toolGlob(input json.RawMessage) string {
	var params struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	json.Unmarshal(input, &params)
	if params.Pattern == "" {
		return jsonError("pattern is required")
	}
	if params.Path == "" {
		params.Path = workspaceRoot
	}
	basePath, err := resolvePath(params.Path)
	if err != nil {
		return jsonError(err.Error())
	}

	var matches []string
	truncated := false

	filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= 200 {
			truncated = true
			return filepath.SkipAll
		}

		rel, _ := filepath.Rel(basePath, path)
		if rel == "." {
			return nil
		}

		if matchGlob(params.Pattern, rel, d.IsDir()) {
			matches = append(matches, rel)
		}
		return nil
	})

	return jsonResult(map[string]interface{}{
		"pattern":   params.Pattern,
		"count":     len(matches),
		"files":     matches,
		"truncated": truncated,
	})
}

// matchGlob matches a relative path against a glob pattern with ** support.
func matchGlob(pattern, path string, isDir bool) bool {
	if !strings.Contains(pattern, "**") {
		// Simple glob — match full relative path
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
		// Also try matching just the filename
		if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
			return true
		}
		return false
	}

	// Handle ** patterns (e.g. "**/*.go", "src/**/*.ts")
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(parts[0], string(filepath.Separator))
	suffix := strings.TrimPrefix(parts[1], string(filepath.Separator))

	// Check prefix
	if prefix != "" {
		if !strings.HasPrefix(path, prefix+string(filepath.Separator)) && path != prefix {
			return false
		}
	}

	// No suffix — ** alone matches everything
	if suffix == "" {
		return !isDir
	}

	// Match suffix against the filename or remaining path segments
	remaining := path
	if prefix != "" {
		remaining = strings.TrimPrefix(path, prefix+string(filepath.Separator))
		if remaining == path {
			remaining = strings.TrimPrefix(path, prefix)
		}
	}

	// Try matching the suffix against just the filename
	if matched, _ := filepath.Match(suffix, filepath.Base(path)); matched {
		return true
	}
	// Try matching the suffix against the remaining path
	if matched, _ := filepath.Match(suffix, remaining); matched {
		return true
	}
	return false
}

func toolGrep(input json.RawMessage) string {
	var params struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	json.Unmarshal(input, &params)
	if params.Pattern == "" {
		return jsonError("pattern is required")
	}
	if params.Path == "" {
		params.Path = workspaceRoot
	}
	basePath, err := resolvePath(params.Path)
	if err != nil {
		return jsonError(err.Error())
	}

	re, err := regexp.Compile(params.Pattern)
	if err != nil {
		return jsonError(fmt.Sprintf("invalid regex: %v", err))
	}

	type grepMatch struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	var matches []grepMatch
	truncated := false

	filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if truncated {
			return filepath.SkipAll
		}

		// Skip large files (>1MB)
		info, _ := d.Info()
		if info != nil && info.Size() > 1024*1024 {
			return nil
		}

		rel, _ := filepath.Rel(basePath, path)

		// Apply include filter
		if params.Include != "" {
			if matched, _ := filepath.Match(params.Include, filepath.Base(path)); !matched {
				return nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				text := line
				if len(text) > 200 {
					text = text[:200] + "..."
				}
				matches = append(matches, grepMatch{File: rel, Line: i + 1, Text: text})
				if len(matches) >= 50 {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	return jsonResult(map[string]interface{}{
		"pattern":   params.Pattern,
		"count":     len(matches),
		"matches":   matches,
		"truncated": truncated,
	})
}

// Precompiled regexes for HTML stripping.
var (
	reScript   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reNav      = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`)
	reFooter   = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`)
	reTags     = regexp.MustCompile(`<[^>]+>`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reNewlines = regexp.MustCompile(`\n{3,}`)
)

func toolWebFetch(input json.RawMessage) string {
	var params struct {
		URL string `json:"url"`
	}
	json.Unmarshal(input, &params)
	if params.URL == "" {
		return jsonError("url is required")
	}
	if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
		return jsonError("url must start with http:// or https://")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(params.URL)
	if err != nil {
		return jsonError(fmt.Sprintf("fetch failed: %v", err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB limit
	if err != nil {
		return jsonError(fmt.Sprintf("read body: %v", err))
	}

	contentType := resp.Header.Get("Content-Type")
	text := string(body)
	truncated := false

	if strings.Contains(contentType, "html") {
		text = stripHTML(text)
	}

	if len(text) > 10000 {
		text = text[:10000]
		truncated = true
	}

	return jsonResult(map[string]interface{}{
		"url":          params.URL,
		"status":       resp.StatusCode,
		"content_type": contentType,
		"text":         text,
		"truncated":    truncated,
	})
}

// stripHTML removes HTML tags and extracts readable text.
func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, "")
	s = reStyle.ReplaceAllString(s, "")
	s = reNav.ReplaceAllString(s, "")
	s = reFooter.ReplaceAllString(s, "")
	s = reTags.ReplaceAllString(s, "")

	// Decode common HTML entities
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")

	// Normalize whitespace
	s = reSpaces.ReplaceAllString(s, " ")
	s = reNewlines.ReplaceAllString(s, "\n\n")

	return strings.TrimSpace(s)
}

func (a *Agent) toolCronCreate(input json.RawMessage) string {
	var params struct {
		Schedule   string `json:"schedule"`
		Message    string `json:"message"`
		Session    string `json:"session"`
		OnConflict string `json:"on_conflict"`
	}
	json.Unmarshal(input, &params)
	if a.cron == nil {
		return jsonError("cron not initialized")
	}
	id, err := a.cron.Create(params.Schedule, params.Message, params.Session, params.OnConflict)
	if err != nil {
		return jsonError(err.Error())
	}
	tz, _ := time.Now().Zone()
	return jsonResult(map[string]interface{}{
		"ok":   true,
		"id":   id,
		"note": fmt.Sprintf("schedule evaluated in host local time (%s)", tz),
	})
}

func (a *Agent) toolCronList(input json.RawMessage) string {
	if a.cron == nil {
		return jsonError("cron not initialized")
	}
	entries, err := a.cron.List()
	if err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{
		"count":   len(entries),
		"entries": entries,
	})
}

func (a *Agent) toolCronDelete(input json.RawMessage) string {
	var params struct{ ID string `json:"id"` }
	json.Unmarshal(input, &params)
	if a.cron == nil {
		return jsonError("cron not initialized")
	}
	if params.ID == "" {
		return jsonError("id is required")
	}
	if err := a.cron.Delete(params.ID); err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{"ok": true})
}

func (a *Agent) toolCronEnable(input json.RawMessage) string {
	var params struct{ ID string `json:"id"` }
	json.Unmarshal(input, &params)
	if a.cron == nil {
		return jsonError("cron not initialized")
	}
	if params.ID == "" {
		return jsonError("id is required")
	}
	if err := a.cron.SetEnabled(params.ID, true); err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{"ok": true, "enabled": true})
}

func (a *Agent) toolCronDisable(input json.RawMessage) string {
	var params struct{ ID string `json:"id"` }
	json.Unmarshal(input, &params)
	if a.cron == nil {
		return jsonError("cron not initialized")
	}
	if params.ID == "" {
		return jsonError("id is required")
	}
	if err := a.cron.SetEnabled(params.ID, false); err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{"ok": true, "enabled": false})
}

func (a *Agent) toolMemoryStore(input json.RawMessage) string {
	var params struct {
		Text  string   `json:"text"`
		Tags  []string `json:"tags"`
		Scope string   `json:"scope"`
	}
	json.Unmarshal(input, &params)
	if a.memory == nil {
		return jsonError("memory not initialized")
	}
	id, err := a.memory.Store(params.Text, params.Tags, params.Scope)
	if err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{"ok": true, "id": id})
}

func (a *Agent) toolMemorySearch(input json.RawMessage) string {
	var params struct {
		Query string `json:"query"`
		Tag   string `json:"tag"`
	}
	json.Unmarshal(input, &params)
	if a.memory == nil {
		return jsonError("memory not initialized")
	}
	matches := a.memory.Search(params.Query, params.Tag)
	return jsonResult(map[string]interface{}{
		"count":    len(matches),
		"memories": matches,
	})
}

func (a *Agent) toolMemoryDelete(input json.RawMessage) string {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(input, &params)
	if a.memory == nil {
		return jsonError("memory not initialized")
	}
	if params.ID == "" {
		return jsonError("id is required")
	}
	if err := a.memory.Delete(params.ID); err != nil {
		return jsonError(err.Error())
	}
	return jsonResult(map[string]interface{}{"ok": true})
}

// jsonResult marshals a value to JSON string.
func jsonResult(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// jsonError returns a JSON error object.
func jsonError(msg string) string {
	return jsonResult(map[string]interface{}{"ok": false, "error": msg})
}
