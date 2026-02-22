package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Session tracks a conversation with a specific channel+ID pair.
type Session struct {
	mu       sync.Mutex
	key      string
	filePath string
	turns    []Turn
}

// Turn is a single conversation turn.
type Turn struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string or []ContentBlock
	TS         string      `json:"ts"`
	User       string      `json:"user,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

func (a *Agent) getOrCreateSession(sid SessionID) *Session {
	key := sid.Channel + "_" + sid.ID
	a.mu.Lock()
	defer a.mu.Unlock()
	if sess, ok := a.sessions[key]; ok {
		return sess
	}
	sess := &Session{key: key, filePath: filepath.Join(sessionsDir, key+".jsonl")}
	sess.loadFromDisk()
	a.sessions[key] = sess
	return sess
}

func (s *Session) appendTurn(turn Turn) {
	if turn.TS == "" {
		turn.TS = now()
	}
	s.mu.Lock()
	s.turns = append(s.turns, turn)
	s.mu.Unlock()

	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(turn)
	f.Write(append(data, '\n'))
}

func (s *Session) loadFromDisk() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
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

// assembleContext builds the message list for the LLM: system prompt + last N turns.
func (s *Session) assembleContext(systemPrompt string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	var selected []Turn
	totalChars := 0
	for i := len(s.turns) - 1; i >= 0 && len(selected) < maxContextTurns; i-- {
		t := s.turns[i]
		size := turnSize(t)
		if totalChars+size > maxContextChars {
			break
		}
		totalChars += size
		selected = append([]Turn{t}, selected...)
	}

	messages := []Message{{Role: "system", Content: systemPrompt}}
	for _, t := range selected {
		messages = append(messages, Message{
			Role: t.Role, Content: t.Content, ToolCallID: t.ToolCallID,
		})
	}
	return messages
}

func turnSize(t Turn) int {
	switch v := t.Content.(type) {
	case string:
		return len(v)
	default:
		data, _ := json.Marshal(v)
		return len(data)
	}
}
