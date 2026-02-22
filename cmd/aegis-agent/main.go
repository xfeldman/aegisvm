// aegis-agent is the guest agent runtime for the Aegis Agent Kit.
//
// It runs inside the VM, receives tether frames from the harness,
// manages sessions, calls LLM APIs, and streams responses back.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o aegis-agent ./cmd/aegis-agent
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	listenAddr     = "127.0.0.1:7778" // receives tether frames from harness
	harnessAPI     = "http://127.0.0.1:7777"
	sessionsDir    = "/workspace/sessions"
	maxContextTurns = 50
	maxContextChars = 24000 // ~6K tokens
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("aegis-agent starting")

	os.MkdirAll(sessionsDir, 0755)

	agent := &Agent{
		sessions: make(map[string]*Session),
	}

	// Determine LLM provider
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		agent.llm = &ClaudeLLM{apiKey: key}
		log.Println("LLM provider: Claude")
	} else if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		agent.llm = &OpenAILLM{apiKey: key}
		log.Println("LLM provider: OpenAI")
	} else {
		log.Println("WARNING: no LLM API key set (ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	}

	// Read optional system prompt
	agent.systemPrompt = os.Getenv("AEGIS_SYSTEM_PROMPT")
	if agent.systemPrompt == "" {
		agent.systemPrompt = "You are a helpful assistant."
	}

	// HTTP server for receiving tether frames from harness
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tether/recv", agent.handleTetherRecv)

	server := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		log.Printf("agent listening on %s", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agent server: %v", err)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	log.Println("aegis-agent stopped")
}

// Agent is the main agent runtime.
type Agent struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	llm          LLM
	systemPrompt string
}

// Session tracks a conversation with a specific channel+ID pair.
type Session struct {
	mu       sync.Mutex
	key      string
	filePath string
	turns    []Turn
}

// Turn is a single conversation turn.
type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      string `json:"ts"`
}

// TetherFrame matches the tether.Frame type.
type TetherFrame struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	TS      string          `json:"ts,omitempty"`
	Session SessionID       `json:"session"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SessionID matches the tether.SessionID type.
type SessionID struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

// handleTetherRecv receives a tether frame from the harness.
func (a *Agent) handleTetherRecv(w http.ResponseWriter, r *http.Request) {
	var frame TetherFrame
	if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch frame.Type {
	case "user.message":
		go a.handleUserMessage(frame)
	case "control.cancel":
		// TODO: cancel in-flight LLM request
	}

	w.WriteHeader(http.StatusAccepted)
}

// handleUserMessage processes an incoming user message.
func (a *Agent) handleUserMessage(frame TetherFrame) {
	// Extract text from payload
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		log.Printf("invalid user.message payload: %v", err)
		return
	}

	if payload.Text == "" {
		return
	}

	sess := a.getOrCreateSession(frame.Session)

	// Append user turn
	sess.appendTurn(Turn{
		Role:    "user",
		Content: payload.Text,
		TS:      frame.TS,
	})

	// Send thinking status
	a.sendFrame(TetherFrame{
		V:       1,
		Type:    "status.presence",
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Session: frame.Session,
		Payload: mustMarshal(map[string]string{"state": "thinking"}),
	})

	if a.llm == nil {
		// No LLM configured — echo back
		a.sendDone(frame.Session, "No LLM API key configured. Set ANTHROPIC_API_KEY or OPENAI_API_KEY.")
		sess.appendTurn(Turn{Role: "assistant", Content: "No LLM API key configured.", TS: time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}

	// Assemble context
	messages := sess.assembleContext(a.systemPrompt)

	// Call LLM with streaming
	var fullText strings.Builder
	err := a.llm.StreamChat(context.Background(), messages, func(delta string) {
		fullText.WriteString(delta)
		a.sendFrame(TetherFrame{
			V:       1,
			Type:    "assistant.delta",
			TS:      time.Now().UTC().Format(time.RFC3339Nano),
			Session: frame.Session,
			Payload: mustMarshal(map[string]string{"text": delta}),
		})
	})

	if err != nil {
		log.Printf("LLM error: %v", err)
		errMsg := fmt.Sprintf("LLM error: %v", err)
		a.sendDone(frame.Session, errMsg)
		sess.appendTurn(Turn{Role: "assistant", Content: errMsg, TS: time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}

	// Send done
	finalText := fullText.String()
	a.sendDone(frame.Session, finalText)
	sess.appendTurn(Turn{Role: "assistant", Content: finalText, TS: time.Now().UTC().Format(time.RFC3339Nano)})
}

func (a *Agent) sendDone(session SessionID, text string) {
	a.sendFrame(TetherFrame{
		V:       1,
		Type:    "assistant.done",
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Session: session,
		Payload: mustMarshal(map[string]string{"text": text}),
	})
}

func (a *Agent) sendFrame(frame TetherFrame) {
	data, _ := json.Marshal(frame)
	resp, err := http.Post(harnessAPI+"/v1/tether/send", "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("send tether frame: %v", err)
		return
	}
	resp.Body.Close()
}

func (a *Agent) getOrCreateSession(sid SessionID) *Session {
	key := sid.Channel + "_" + sid.ID

	a.mu.Lock()
	defer a.mu.Unlock()

	if sess, ok := a.sessions[key]; ok {
		return sess
	}

	sess := &Session{
		key:      key,
		filePath: filepath.Join(sessionsDir, key+".jsonl"),
	}

	// Load existing session from disk
	sess.loadFromDisk()

	a.sessions[key] = sess
	return sess
}

func (s *Session) appendTurn(turn Turn) {
	if turn.TS == "" {
		turn.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}

	s.mu.Lock()
	s.turns = append(s.turns, turn)
	s.mu.Unlock()

	// Persist to JSONL
	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("session persist: %v", err)
		return
	}
	defer f.Close()
	data, _ := json.Marshal(turn)
	f.Write(append(data, '\n'))
}

func (s *Session) loadFromDisk() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return // no existing session
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var turn Turn
		if json.Unmarshal(scanner.Bytes(), &turn) == nil {
			s.turns = append(s.turns, turn)
		}
	}
	if len(s.turns) > 0 {
		log.Printf("loaded %d turns from session %s", len(s.turns), s.key)
	}
}

// assembleContext builds the LLM message list: system prompt + last N turns within budget.
func (s *Session) assembleContext(systemPrompt string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Start from the end and work backwards within char budget
	var selected []Turn
	totalChars := 0
	for i := len(s.turns) - 1; i >= 0 && len(selected) < maxContextTurns; i-- {
		t := s.turns[i]
		if totalChars+len(t.Content) > maxContextChars {
			break
		}
		totalChars += len(t.Content)
		selected = append([]Turn{t}, selected...)
	}

	messages := make([]Message, 0, len(selected)+1)
	messages = append(messages, Message{Role: "system", Content: systemPrompt})
	for _, t := range selected {
		messages = append(messages, Message{Role: t.Role, Content: t.Content})
	}
	return messages
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// LLM interface

// Message is a chat message for the LLM.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLM is the interface for LLM providers.
type LLM interface {
	StreamChat(ctx context.Context, messages []Message, onDelta func(string)) error
}

// ClaudeLLM implements the LLM interface using the Anthropic API.
type ClaudeLLM struct {
	apiKey string
}

func (c *ClaudeLLM) StreamChat(ctx context.Context, messages []Message, onDelta func(string)) error {
	// Separate system message from conversation
	var system string
	var chatMessages []map[string]string
	for _, m := range messages {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		chatMessages = append(chatMessages, map[string]string{
			"role":    m.Role,
			"content": m.Content,
		})
	}

	body := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"stream":     true,
		"messages":   chatMessages,
	}
	if system != "" {
		body["system"] = system
	}

	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("claude API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("claude API %d: %s", resp.StatusCode, string(body))
	}

	return parseClaudeSSE(resp.Body, onDelta)
}

func parseClaudeSSE(r io.Reader, onDelta func(string)) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			onDelta(event.Delta.Text)
		}
	}
	return scanner.Err()
}

// OpenAILLM implements the LLM interface using the OpenAI API.
type OpenAILLM struct {
	apiKey string
}

func (o *OpenAILLM) StreamChat(ctx context.Context, messages []Message, onDelta func(string)) error {
	var chatMessages []map[string]string
	for _, m := range messages {
		chatMessages = append(chatMessages, map[string]string{
			"role":    m.Role,
			"content": m.Content,
		})
	}

	body := map[string]interface{}{
		"model":    "gpt-4o",
		"stream":   true,
		"messages": chatMessages,
	}

	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("openai API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openai API %d: %s", resp.StatusCode, string(body))
	}

	return parseOpenAISSE(resp.Body, onDelta)
}

func parseOpenAISSE(r io.Reader, onDelta func(string)) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if len(event.Choices) > 0 && event.Choices[0].Delta.Content != "" {
			onDelta(event.Choices[0].Delta.Content)
		}
	}
	return scanner.Err()
}
