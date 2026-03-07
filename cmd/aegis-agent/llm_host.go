package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HostLLM implements LLM by proxying requests through the harness to a host-local
// inference server (Ollama, LM Studio, vLLM). Streaming response chunks arrive
// as llm.frame notifications delivered to the agent's /v1/llm/recv endpoint.
type HostLLM struct {
	provider  string // e.g. "ollama"
	model     string // e.g. "llama3.2"
	maxTokens int

	mu      sync.Mutex
	pending map[string]chan hostLLMFrame // req_id → channel
}

type hostLLMFrame struct {
	Type  string `json:"type"`
	ReqID string `json:"req_id"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func (h *HostLLM) StreamChat(ctx context.Context, messages []Message, tools []Tool, onDelta func(string), onReasoning func(string), onReasoningDone func()) (*LLMResponse, error) {
	reqID := fmt.Sprintf("llm-%d", time.Now().UnixNano())

	// Register pending channel
	ch := make(chan hostLLMFrame, 64)
	h.mu.Lock()
	h.pending[reqID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, reqID)
		h.mu.Unlock()
	}()

	// Build OpenAI-compatible request body
	var chatMessages []map[string]interface{}
	for _, m := range messages {
		msg := map[string]interface{}{"role": m.Role}
		switch v := m.Content.(type) {
		case string:
			if m.Role == "tool" {
				msg["content"] = v
				msg["tool_call_id"] = m.ToolCallID
			} else {
				msg["content"] = v
			}
		default:
			// Reconstruct OpenAI format from stored content blocks
			data, _ := json.Marshal(v)
			var blocks []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text,omitempty"`
				ID    string          `json:"id,omitempty"`
				Name  string          `json:"name,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
			}
			if json.Unmarshal(data, &blocks) == nil {
				var text string
				var tcs []map[string]interface{}
				for _, b := range blocks {
					if b.Type == "text" {
						text += b.Text
					} else if b.Type == "tool_use" {
						inputStr, _ := json.Marshal(b.Input)
						tcs = append(tcs, map[string]interface{}{
							"id": b.ID, "type": "function",
							"function": map[string]interface{}{"name": b.Name, "arguments": string(inputStr)},
						})
					}
				}
				msg["content"] = text
				if len(tcs) > 0 {
					msg["tool_calls"] = tcs
				}
			} else {
				msg["content"] = fmt.Sprint(v)
			}
		}
		chatMessages = append(chatMessages, msg)
	}

	body := map[string]interface{}{
		"model":    h.model,
		"stream":   true,
		"messages": chatMessages,
	}
	if h.maxTokens > 0 {
		body["max_tokens"] = h.maxTokens
	}
	if len(tools) > 0 {
		var oaiTools []map[string]interface{}
		for _, t := range tools {
			oaiTools = append(oaiTools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
				},
			})
		}
		body["tools"] = oaiTools
	}

	bodyJSON, _ := json.Marshal(body)

	// Send request to harness → aegisd (include req_id so aegisd uses ours)
	reqBody, _ := json.Marshal(map[string]interface{}{
		"provider": h.provider,
		"model":    h.model,
		"req_id":   reqID,
		"body":     json.RawMessage(bodyJSON),
	})

	resp, err := http.Post(harnessAPI+"/v1/llm/chat", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("host llm: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("host llm: %s", errResp.Error.Message)
	}

	// Parse response to confirm req_id
	var rpcResult struct {
		ReqID string `json:"req_id"`
	}
	json.NewDecoder(resp.Body).Decode(&rpcResult)
	if rpcResult.ReqID == "" {
		return nil, fmt.Errorf("host llm: no req_id in response")
	}

	// Wait for streaming frames
	llmResp := &LLMResponse{}

	type partialTC struct {
		id   string
		name string
		args strings.Builder
	}
	toolCalls := make(map[int]*partialTC)
	wasReasoning := false

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("host llm: channel closed")
			}

			switch frame.Type {
			case "llm.delta":
				// Parse OpenAI streaming chunk
				var event struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							Reasoning string `json:"reasoning"` // Ollama reasoning field
							ToolCalls []struct {
								Index    int    `json:"index"`
								ID       string `json:"id"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(frame.Data), &event) != nil || len(event.Choices) == 0 {
					continue
				}

				delta := event.Choices[0].Delta
				if delta.Reasoning != "" && onReasoning != nil {
					onReasoning(delta.Reasoning)
					wasReasoning = true
				}
				if delta.Content != "" {
					if wasReasoning && onReasoningDone != nil {
						onReasoningDone()
						wasReasoning = false
					}
					onDelta(delta.Content)
				}

				for _, tc := range delta.ToolCalls {
					ptc, ok := toolCalls[tc.Index]
					if !ok {
						ptc = &partialTC{}
						toolCalls[tc.Index] = ptc
					}
					if tc.ID != "" {
						ptc.id = tc.ID
					}
					if tc.Function.Name != "" {
						ptc.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						ptc.args.WriteString(tc.Function.Arguments)
					}
				}

			case "llm.done":
				// Assemble tool calls
				for i := 0; i < len(toolCalls); i++ {
					ptc := toolCalls[i]
					if ptc == nil {
						continue
					}
					inputJSON := json.RawMessage(ptc.args.String())
					if len(inputJSON) == 0 {
						inputJSON = json.RawMessage("{}")
					}
					llmResp.ToolCalls = append(llmResp.ToolCalls, ToolCall{
						ID: ptc.id, Name: ptc.name, Input: inputJSON,
					})
				}
				if len(llmResp.ToolCalls) > 0 {
					var blocks []interface{}
					for _, tc := range llmResp.ToolCalls {
						blocks = append(blocks, map[string]interface{}{
							"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Input,
						})
					}
					llmResp.RawContent = blocks
				}
				return llmResp, nil

			case "llm.error":
				return nil, fmt.Errorf("host llm: %s", frame.Error)
			}
		}
	}
}

// handleLLMRecv receives llm.frame notifications from the harness.
func (a *Agent) handleLLMRecv(w http.ResponseWriter, r *http.Request) {
	var frame hostLLMFrame
	if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if a.hostLLM != nil {
		a.hostLLM.routeFrame(frame)
	}
	w.WriteHeader(http.StatusAccepted)
}

// routeFrame delivers an incoming LLM frame to the waiting StreamChat call.
func (h *HostLLM) routeFrame(frame hostLLMFrame) {
	h.mu.Lock()
	ch, ok := h.pending[frame.ReqID]
	h.mu.Unlock()
	if ok {
		select {
		case ch <- frame:
		default:
			log.Printf("host llm: channel full for %s, dropping frame", frame.ReqID)
		}
	}
	// Straggler frames after StreamChat returns are expected — don't log.
}
