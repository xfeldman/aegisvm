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
	memory          *MemoryStore
	cron            *CronStore
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

	claudeKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")

	switch {
	case provider == "claude" || provider == "anthropic":
		agent.llm = &ClaudeLLM{apiKey: claudeKey, model: modelName, maxTokens: maxTokens}
		log.Printf("LLM provider: Claude (model=%s)", modelName)
	case provider == "openai":
		agent.llm = &OpenAILLM{apiKey: openaiKey, model: modelName, maxTokens: maxTokens}
		log.Printf("LLM provider: OpenAI (model=%s)", modelName)
	case claudeKey != "":
		agent.llm = &ClaudeLLM{apiKey: claudeKey, model: modelName, maxTokens: maxTokens}
		log.Println("LLM provider: Claude")
	case openaiKey != "":
		agent.llm = &OpenAILLM{apiKey: openaiKey, model: modelName, maxTokens: maxTokens}
		log.Println("LLM provider: OpenAI")
	default:
		log.Println("WARNING: no LLM API key set (ANTHROPIC_API_KEY or OPENAI_API_KEY)")
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
			a.sendDone(frame.Session, text)
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

		a.sendPresence(frame.Session, "thinking")
	}

	msg := "Tool loop limit reached."
	a.sendDone(frame.Session, msg)
	sess.appendTurn(Turn{Role: "assistant", Content: msg, TS: now()})
}

const defaultSystemPrompt = `You are a helpful assistant running inside an Aegis VM. Your persistent workspace is at /workspace/. You have tools for running commands, reading/writing files, and optionally managing child VM instances. Read tool descriptions carefully — they explain parameters and constraints.

You have persistent memory tools. Use memory_store when:
- The user explicitly asks you to remember something
- You learn a stable fact about the user or project that will be useful across sessions
Do NOT store: transient task context, secrets/tokens, or information already in files.
Use memory_delete to remove outdated memories. Memories are automatically surfaced in your context when relevant.`

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
