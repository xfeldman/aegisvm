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
// MCP tools are configured via /workspace/.aegis/mcp.json.
// If no config exists, aegis-mcp-guest is auto-discovered from the rootfs.
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
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	listenAddr      = "127.0.0.1:7778"
	harnessAPI      = "http://127.0.0.1:7777"
	sessionsDir     = "/workspace/sessions"
	workspaceRoot   = "/workspace"
	maxContextTurns = 50
	maxContextChars = 24000
	maxToolRounds   = 20
)

// Agent is the main agent runtime.
type Agent struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	llm          LLM
	systemPrompt string
	mcpClients   map[string]*MCPClient
	allTools     []Tool
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("aegis-agent starting")

	os.MkdirAll(sessionsDir, 0755)

	agent := &Agent{
		sessions: make(map[string]*Session),
	}

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		agent.llm = &ClaudeLLM{apiKey: key}
		log.Println("LLM provider: Claude")
	} else if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		agent.llm = &OpenAILLM{apiKey: key}
		log.Println("LLM provider: OpenAI")
	} else {
		log.Println("WARNING: no LLM API key set (ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	}

	agent.systemPrompt = os.Getenv("AEGIS_SYSTEM_PROMPT")
	if agent.systemPrompt == "" {
		agent.systemPrompt = defaultSystemPrompt
	}

	agent.initMCPTools()

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
		Text string `json:"text"`
		User *struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil || payload.Text == "" {
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

	sess.appendTurn(Turn{Role: "user", Content: content, TS: frame.TS, User: userName})
	a.sendPresence(frame.Session, "thinking")

	if a.llm == nil {
		msg := "No LLM API key configured. Set ANTHROPIC_API_KEY or OPENAI_API_KEY."
		a.sendDone(frame.Session, msg)
		sess.appendTurn(Turn{Role: "assistant", Content: msg, TS: now()})
		return
	}

	// Agentic loop: call LLM with streaming, execute tools, repeat
	for round := 0; round < maxToolRounds; round++ {
		messages := sess.assembleContext(a.systemPrompt)

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

const defaultSystemPrompt = `You are a helpful assistant running inside an Aegis VM. Your persistent workspace is at /workspace/. You have tools for running commands, reading/writing files, and optionally managing child VM instances. Read tool descriptions carefully — they explain parameters and constraints.`

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
