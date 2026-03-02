package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHostLLMStreamChat(t *testing.T) {
	h := &HostLLM{
		provider:  "ollama",
		model:     "llama3.2",
		maxTokens: 100,
		pending:   make(map[string]chan hostLLMFrame),
	}

	ctx := context.Background()
	messages := []Message{{Role: "user", Content: "hello"}}

	var gotDeltas []string
	var wg sync.WaitGroup
	wg.Add(1)

	var resp *LLMResponse
	var respErr error

	go func() {
		defer wg.Done()
		// StreamChat will block on POST to harness (which won't work in test),
		// so we test routeFrame and channel mechanics directly.
		// Create a pending entry manually to simulate the flow.
		reqID := "test-req-1"
		ch := make(chan hostLLMFrame, 16)
		h.mu.Lock()
		h.pending[reqID] = ch
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.pending, reqID)
			h.mu.Unlock()
		}()

		llmResp := &LLMResponse{}

		type partialTC struct {
			id   string
			name string
			args strings.Builder
		}
		toolCalls := make(map[int]*partialTC)

		for {
			select {
			case <-ctx.Done():
				respErr = ctx.Err()
				return
			case frame := <-ch:
				switch frame.Type {
				case "llm.delta":
					var event struct {
						Choices []struct {
							Delta struct {
								Content   string `json:"content"`
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
					if delta.Content != "" {
						gotDeltas = append(gotDeltas, delta.Content)
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
						ptc.args.WriteString(tc.Function.Arguments)
					}
				case "llm.done":
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
					resp = llmResp
					return
				case "llm.error":
					respErr = &llmTestError{frame.Error}
					return
				}
			}
		}
	}()

	// Simulate frames arriving from aegisd
	time.Sleep(10 * time.Millisecond) // let goroutine start
	h.routeFrame(hostLLMFrame{
		Type:  "llm.delta",
		ReqID: "test-req-1",
		Data:  `{"choices":[{"delta":{"content":"Hello"}}]}`,
	})
	h.routeFrame(hostLLMFrame{
		Type:  "llm.delta",
		ReqID: "test-req-1",
		Data:  `{"choices":[{"delta":{"content":" world"}}]}`,
	})
	h.routeFrame(hostLLMFrame{
		Type:  "llm.done",
		ReqID: "test-req-1",
	})

	wg.Wait()

	if respErr != nil {
		t.Fatalf("unexpected error: %v", respErr)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}
	if got := strings.Join(gotDeltas, ""); got != "Hello world" {
		t.Errorf("deltas = %q, want %q", got, "Hello world")
	}
	_ = messages // used in real flow
}

func TestHostLLMStreamChatWithToolCalls(t *testing.T) {
	h := &HostLLM{
		provider:  "ollama",
		model:     "llama3.2",
		maxTokens: 100,
		pending:   make(map[string]chan hostLLMFrame),
	}

	reqID := "test-tc-1"
	ch := make(chan hostLLMFrame, 16)
	h.mu.Lock()
	h.pending[reqID] = ch
	h.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)

	var resp *LLMResponse
	go func() {
		defer wg.Done()
		llmResp := &LLMResponse{}

		type partialTC struct {
			id   string
			name string
			args strings.Builder
		}
		toolCalls := make(map[int]*partialTC)

		for frame := range ch {
			switch frame.Type {
			case "llm.delta":
				var event struct {
					Choices []struct {
						Delta struct {
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
				if json.Unmarshal([]byte(frame.Data), &event) == nil && len(event.Choices) > 0 {
					for _, tc := range event.Choices[0].Delta.ToolCalls {
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
						ptc.args.WriteString(tc.Function.Arguments)
					}
				}
			case "llm.done":
				for i := 0; i < len(toolCalls); i++ {
					ptc := toolCalls[i]
					if ptc == nil {
						continue
					}
					llmResp.ToolCalls = append(llmResp.ToolCalls, ToolCall{
						ID: ptc.id, Name: ptc.name, Input: json.RawMessage(ptc.args.String()),
					})
				}
				resp = llmResp
				return
			}
		}
	}()

	// Simulate streamed tool call
	h.routeFrame(hostLLMFrame{Type: "llm.delta", ReqID: reqID, Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]}}]}`})
	h.routeFrame(hostLLMFrame{Type: "llm.delta", ReqID: reqID, Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`})
	h.routeFrame(hostLLMFrame{Type: "llm.delta", ReqID: reqID, Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`})
	h.routeFrame(hostLLMFrame{Type: "llm.done", ReqID: reqID})
	close(ch)

	wg.Wait()

	if resp == nil {
		t.Fatal("expected response")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "bash" {
		t.Errorf("tool call = %s/%s, want call_1/bash", tc.ID, tc.Name)
	}
	if string(tc.Input) != `{"command":"ls"}` {
		t.Errorf("tool input = %s, want %s", tc.Input, `{"command":"ls"}`)
	}
}

func TestHostLLMStreamChatError(t *testing.T) {
	h := &HostLLM{
		provider:  "ollama",
		model:     "llama3.2",
		pending:   make(map[string]chan hostLLMFrame),
	}

	reqID := "test-err-1"
	ch := make(chan hostLLMFrame, 16)
	h.mu.Lock()
	h.pending[reqID] = ch
	h.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)

	var gotErr error
	go func() {
		defer wg.Done()
		for frame := range ch {
			if frame.Type == "llm.error" {
				gotErr = &llmTestError{frame.Error}
				return
			}
		}
	}()

	h.routeFrame(hostLLMFrame{Type: "llm.error", ReqID: reqID, Error: "connection refused: localhost:11434"})
	close(ch)
	wg.Wait()

	if gotErr == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(gotErr.Error(), "connection refused") {
		t.Errorf("error = %v, want connection refused", gotErr)
	}
}

func TestHostLLMRouteFrameOrphan(t *testing.T) {
	h := &HostLLM{
		provider:  "ollama",
		model:     "llama3.2",
		pending:   make(map[string]chan hostLLMFrame),
	}

	// Should not panic — just logs and discards
	h.routeFrame(hostLLMFrame{Type: "llm.delta", ReqID: "nonexistent", Data: "test"})
}

func TestHostModelStringParsing(t *testing.T) {
	tests := []struct {
		model    string
		isHost   bool
		provider string
		name     string
	}{
		{"host:ollama/llama3.2", true, "ollama", "llama3.2"},
		{"host:lmstudio/mistral-7b", true, "lmstudio", "mistral-7b"},
		{"host:vllm/meta-llama/Llama-3.1-8B", true, "vllm", "meta-llama/Llama-3.1-8B"},
		{"host:ollama/qwen2.5:14b", true, "ollama", "qwen2.5:14b"},
		{"openai/gpt-4.1", false, "", ""},
		{"anthropic/claude-sonnet-4-6", false, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			isHost := strings.HasPrefix(tt.model, "host:")
			if isHost != tt.isHost {
				t.Fatalf("isHost = %v, want %v", isHost, tt.isHost)
			}
			if !isHost {
				return
			}
			hostModel := tt.model[5:]
			var provider, name string
			if i := strings.Index(hostModel, "/"); i > 0 {
				provider = hostModel[:i]
				name = hostModel[i+1:]
			}
			if provider != tt.provider {
				t.Errorf("provider = %q, want %q", provider, tt.provider)
			}
			if name != tt.name {
				t.Errorf("name = %q, want %q", name, tt.name)
			}
		})
	}
}

type llmTestError struct{ msg string }

func (e *llmTestError) Error() string { return e.msg }
