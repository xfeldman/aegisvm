// aegis-agent is the guest agent runtime for the Aegis Agent Kit.
//
// It runs inside the VM, receives tether frames from the harness,
// manages sessions, calls LLM APIs with tool use, and streams responses back.
//
// Built-in tools (scoped to /workspace/):
//   - bash: execute shell commands
//   - read_file: read file contents
//   - write_file: write file contents
//   - list_files: list directory contents
//
// Agent configuration is loaded from /workspace/.aegis/agent.json.
// MCP tools are auto-discovered from aegis-mcp-guest if no config exists.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o aegis-agent ./cmd/aegis-agent
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	listenAddr    = "127.0.0.1:7778"
	harnessAPI    = "http://127.0.0.1:7777"
	sessionsDir   = "/workspace/sessions"
	workspaceRoot = "/workspace"
	maxToolRounds = 20
)

// Agent is the main agent runtime.
type Agent struct {
	mu              sync.Mutex
	sessions        map[string]*Session
	llm             LLM
	systemPrompt    string
	mcpClients      map[string]*MCPClient
	allTools        []Tool
	maxContextTurns int
	maxContextChars int
	toolsConfig     map[string]ToolConfig
	memory          *MemoryStore
	cron            *CronStore
	pendingImages   []ImageRef // images queued by respond_with_image during current turn
	restartPending  bool       // set by self_restart tool, checked after tool round
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("aegis-agent starting")

	os.MkdirAll(sessionsDir, 0755)

	config := loadAgentConfig()

	// Apply defaults for context limits
	maxTurns := config.ContextTurns
	if maxTurns == 0 {
		maxTurns = 50
	}
	maxChars := config.ContextChars
	if maxChars == 0 {
		maxChars = 24000
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	agent := &Agent{
		sessions:        make(map[string]*Session),
		maxContextTurns: maxTurns,
		maxContextChars: maxChars,
	}

	// Parse model config: "provider/model" or just "model-name"
	provider, modelName := "", ""
	if config.Model != "" {
		if i := strings.Index(config.Model, "/"); i > 0 {
			provider = config.Model[:i]
			modelName = config.Model[i+1:]
		} else {
			modelName = config.Model
		}
	}

	// Resolve API key from config.api_key_env — no magic env var sniffing
	var apiKey string
	if config.APIKeyEnv != "" {
		apiKey = os.Getenv(config.APIKeyEnv)
		if apiKey == "" {
			log.Printf("WARNING: api_key_env=%q is set but env var is empty", config.APIKeyEnv)
		}
		// Auto-detect provider from env var name if not set via model prefix
		if provider == "" {
			switch {
			case strings.Contains(strings.ToLower(config.APIKeyEnv), "anthropic"):
				provider = "anthropic"
			case strings.Contains(strings.ToLower(config.APIKeyEnv), "openai"):
				provider = "openai"
			}
		}
	}

	switch {
	case provider == "claude" || provider == "anthropic":
		agent.llm = &ClaudeLLM{apiKey: apiKey, model: modelName, maxTokens: maxTokens}
		log.Printf("LLM provider: Claude (model=%s)", modelName)
	case provider == "openai":
		agent.llm = &OpenAILLM{apiKey: apiKey, model: modelName, maxTokens: maxTokens}
		log.Printf("LLM provider: OpenAI (model=%s)", modelName)
	case apiKey != "":
		agent.llm = &OpenAILLM{apiKey: apiKey, model: modelName, maxTokens: maxTokens}
		log.Printf("LLM provider: OpenAI-compatible (model=%s)", modelName)
	default:
		log.Println("WARNING: no LLM API key — set api_key_env in agent.json and pass the key via --env")
	}

	agent.systemPrompt = config.SystemPrompt
	if agent.systemPrompt == "" {
		agent.systemPrompt = defaultSystemPrompt
	}

	agent.memory = NewMemoryStore(
		filepath.Join(workspaceRoot, ".aegis", "memory"),
		config.Memory,
	)

	agent.cron = NewCronStore(filepath.Join(workspaceRoot, ".aegis"))

	// Build dynamic system prompt additions
	agent.systemPrompt += "\n\n" + agent.envSummary()

	agent.toolsConfig = config.Tools
	agent.initMCPTools(config)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tether/recv", agent.handleTetherRecv)

	server := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		log.Printf("agent listening on %s", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agent server: %v", err)
		}
	}()

	// Check for pending restart notification
	go agent.sendRestartNotification()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	agent.closeMCP()
	log.Println("aegis-agent stopped")
}

// handleUserMessage processes an incoming user message through the agentic loop.
func (a *Agent) handleUserMessage(frame TetherFrame) {
	var payload struct {
		Text   string `json:"text"`
		Images []struct {
			MediaType string `json:"media_type"`
			Blob      string `json:"blob"`
			Size      int64  `json:"size"`
		} `json:"images"`
		User *struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return
	}
	if payload.Text == "" && len(payload.Images) == 0 {
		return
	}

	sess := a.getOrCreateSession(frame.Session)

	userName := ""
	content := payload.Text
	if payload.User != nil {
		if payload.User.Name != "" {
			userName = payload.User.Name
		} else if payload.User.Username != "" {
			userName = payload.User.Username
		}
		if userName != "" {
			content = fmt.Sprintf("[%s]: %s", userName, payload.Text)
		}
	}

	// Build turn content: plain string for text-only, content blocks for images
	var turnContent interface{}
	if len(payload.Images) > 0 {
		blocks := []interface{}{
			map[string]interface{}{"type": "text", "text": content},
		}
		for _, img := range payload.Images {
			blocks = append(blocks, map[string]interface{}{
				"type":       "image",
				"media_type": img.MediaType,
				"blob":       img.Blob,
			})
		}
		turnContent = blocks
	} else {
		turnContent = content
	}

	sess.appendTurn(Turn{Role: "user", Content: turnContent, TS: frame.TS, User: userName})
	a.pendingImages = nil // reset for this message
	a.sendPresence(frame.Session, "thinking")

	if a.llm == nil {
		msg := "No LLM API key configured. Set ANTHROPIC_API_KEY or OPENAI_API_KEY."
		a.sendDone(frame.Session, msg)
		sess.appendTurn(Turn{Role: "assistant", Content: msg, TS: now()})
		return
	}

	// Inject relevant memories into system prompt for this message
	sysPrompt := a.systemPrompt
	if a.memory != nil {
		if block := a.memory.InjectBlock(content); block != "" {
			sysPrompt = a.systemPrompt + "\n\n" + block
		}
	}

	// Agentic loop: call LLM with streaming, execute tools, repeat
	for round := 0; round < maxToolRounds; round++ {
		messages := sess.assembleContext(sysPrompt, a.maxContextTurns, a.maxContextChars)

		var fullText strings.Builder
		onDelta := func(delta string) {
			fullText.WriteString(delta)
			a.sendDelta(frame.Session, delta)
		}

		resp, err := a.llm.StreamChat(context.Background(), messages, a.allTools, onDelta)
		if err != nil {
			errMsg := fmt.Sprintf("LLM error: %v", err)
			log.Printf("%s", errMsg)
			a.sendDone(frame.Session, errMsg)
			sess.appendTurn(Turn{Role: "assistant", Content: errMsg, TS: now()})
			return
		}

		if len(resp.ToolCalls) == 0 {
			text := fullText.String()
			if len(a.pendingImages) > 0 {
				a.sendDoneWithImages(frame.Session, text, a.pendingImages)
			} else {
				a.sendDone(frame.Session, text)
			}
			sess.appendTurn(Turn{Role: "assistant", Content: text, TS: now()})
			return
		}

		// Store assistant turn with tool calls
		sess.appendTurn(Turn{Role: "assistant", Content: resp.RawContent, TS: now()})

		// Execute tools
		for _, tc := range resp.ToolCalls {
			a.sendPresence(frame.Session, fmt.Sprintf("tool: %s", tc.Name))
			result := a.executeTool(tc.Name, tc.Input)
			log.Printf("tool %s: %d bytes result", tc.Name, len(result))
			sess.appendTurn(Turn{Role: "tool", Content: result, TS: now(), ToolCallID: tc.ID})
		}

		// Handle self_restart: all tool results are written, now exit cleanly
		if a.restartPending {
			a.restartPending = false
			msg := "Restarting to apply configuration changes..."
			a.sendDone(frame.Session, msg)
			sess.appendTurn(Turn{Role: "assistant", Content: msg, TS: now()})
			writeRestartMarker(frame.Session)
			go func() {
				// Give tether frame time to flush
				time.Sleep(500 * time.Millisecond)
				resp, err := http.Post(harnessAPI+"/v1/self/restart", "application/json", nil)
				if err != nil {
					log.Printf("self_restart: %v", err)
					return
				}
				resp.Body.Close()
			}()
			return
		}

		a.sendPresence(frame.Session, "thinking")
	}

	msg := "Tool loop limit reached."
	a.sendDone(frame.Session, msg)
	sess.appendTurn(Turn{Role: "assistant", Content: msg, TS: now()})
}

const restartMarkerPath = "/workspace/.aegis/restart-pending.json"

// envSummary returns a system prompt fragment listing available API keys and secrets.
func (a *Agent) envSummary() string {
	var keys []string
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		name := parts[0]
		// Only include API-key-like env vars
		if strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_TOKEN") || strings.HasSuffix(name, "_SECRET") {
			keys = append(keys, name)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf("Available secrets/API keys in your environment: %s. MCP servers you start inherit these automatically.", strings.Join(keys, ", "))
}

// writeRestartMarker saves session info so the agent can notify the user after restart.
func writeRestartMarker(session SessionID) {
	data, _ := json.Marshal(session)
	os.MkdirAll(filepath.Dir(restartMarkerPath), 0755)
	os.WriteFile(restartMarkerPath, data, 0644)
}

// sendRestartNotification checks for a restart marker and sends "I'm back" to the session.
func (a *Agent) sendRestartNotification() {
	data, err := os.ReadFile(restartMarkerPath)
	if err != nil {
		return
	}
	os.Remove(restartMarkerPath)

	var session SessionID
	if json.Unmarshal(data, &session) != nil || session.Channel == "" {
		return
	}

	// Wait for tether egress to be ready (gateway needs to reconnect after restart)
	time.Sleep(5 * time.Second)

	log.Printf("sending restart notification to session %s/%s", session.Channel, session.ID)
	a.sendDone(session, "Restart complete. New configuration loaded.")

	sess := a.getOrCreateSession(session)
	sess.appendTurn(Turn{Role: "assistant", Content: "Restart complete. New configuration loaded.", TS: now()})
}

const defaultSystemPrompt = `You are an AI assistant running inside an isolated Aegis microVM. You communicate with the user via a messaging channel (Telegram, tether, etc.). You have root access, full internet, and a persistent workspace at /workspace/.

## Identity
You ARE the agent running inside the VM. Do not ask the user about "their setup" or "their client" — you are the one executing tools and managing your own environment. Act autonomously. When asked to do something, do it — don't ask clarifying questions unless genuinely ambiguous.

## Environment
- Your workspace at /workspace/ persists across restarts.
- Check what's available before choosing a language: if Python is installed, use Python (Flask/FastAPI). If Node.js is installed, use Node (Express). Use "which python3" or "which node" to check.
- Install packages with "pip install" (Python) or "npm install" (Node). Use "apk add" for system packages.
- To start a long-running process (server, daemon): use "nohup CMD > /dev/null 2>&1 & echo $!" to fully detach it. Never use just "CMD &" — it keeps stdout open and blocks the bash tool. After starting, use expose_port to make it reachable.
- IMPORTANT: Make services reboot-resilient. Your VM may be stopped and cold-booted on the next request. Create /workspace/.aegis/startup/ directory and add a startup script at /workspace/.aegis/startup/app.start (chmod +x) to auto-launch your services on boot. This way servers, daemons, and cron jobs survive VM restarts without user intervention. Always do this when creating any long-running service.
- If this is Alpine Linux (musl libc), Playwright/Puppeteer's bundled Chromium won't work. Use system Chromium instead: "apk add chromium" then set PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser before running Playwright.
- Configuration file: /workspace/.aegis/agent.json. Use it to add MCP servers (under "mcp") or configure built-in tools (under "tools": {"tool_name": {"enabled": false}}). Call self_restart after editing to apply changes.

## Image handling
When the user asks you to GENERATE an image (draw, create, make): use image_generate with a detailed prompt. The image is automatically attached to your response.
When the user asks you to FIND an existing image (photo, picture of): use image_search → download with bash/wget → respond_with_image.
NEVER just give links — always download and send the actual image.

## Memory
Use memory_store when the user explicitly asks you to remember something, or when you learn a stable fact useful across sessions. Do NOT store secrets or transient task context. Memories are automatically surfaced in context when relevant.`

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
