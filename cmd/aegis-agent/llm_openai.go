package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAILLM implements LLM using the OpenAI Chat Completions API with streaming.
type OpenAILLM struct {
	apiKey string
}

func (o *OpenAILLM) StreamChat(ctx context.Context, messages []Message, tools []Tool, onDelta func(string)) (*LLMResponse, error) {
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
		"model":    "gpt-4o",
		"stream":   true,
		"messages": chatMessages,
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

	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai API %d: %s", resp.StatusCode, string(body))
	}

	return parseOpenAIStream(resp.Body, onDelta)
}

func parseOpenAIStream(r io.Reader, onDelta func(string)) (*LLMResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	llmResp := &LLMResponse{}

	type partialTC struct {
		id   string
		name string
		args strings.Builder
	}
	toolCalls := make(map[int]*partialTC)

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
		if json.Unmarshal([]byte(data), &event) != nil || len(event.Choices) == 0 {
			continue
		}

		delta := event.Choices[0].Delta

		if delta.Content != "" {
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
	}

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

	return llmResp, scanner.Err()
}
