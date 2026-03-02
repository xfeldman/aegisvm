package lifecycle

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleLLMChatUnknownProvider(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-1", []string{"echo"}, nil)
	ch := newMockChannel()
	inst.demuxer = newChannelDemuxer(ch, nil)
	defer inst.demuxer.Stop()

	params, _ := json.Marshal(map[string]interface{}{
		"provider": "unknown",
		"model":    "test",
		"body":     json.RawMessage(`{}`),
	})

	_, err := m.handleLLMChat(inst, params)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown host LLM provider") {
		t.Errorf("error = %v, want 'unknown host LLM provider'", err)
	}
}

func TestHandleLLMChatReturnsReqID(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-2", []string{"echo"}, nil)
	ch := newMockChannel()
	inst.demuxer = newChannelDemuxer(ch, nil)
	defer inst.demuxer.Stop()

	// Override provider map to point to a test server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer srv.Close()

	origOllama := hostLLMProviders["ollama"]
	hostLLMProviders["ollama"] = srv.URL
	defer func() { hostLLMProviders["ollama"] = origOllama }()

	params, _ := json.Marshal(map[string]interface{}{
		"provider": "ollama",
		"model":    "test",
		"body":     json.RawMessage(`{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	})

	result, err := m.handleLLMChat(inst, params)
	if err != nil {
		t.Fatalf("handleLLMChat: %v", err)
	}

	resultMap, ok := result.(map[string]string)
	if !ok {
		t.Fatalf("result type = %T, want map[string]string", result)
	}
	if resultMap["req_id"] == "" {
		t.Fatal("expected non-empty req_id")
	}
	if !strings.HasPrefix(resultMap["req_id"], "llm-") {
		t.Errorf("req_id = %q, want llm- prefix", resultMap["req_id"])
	}
}

func TestProxyLLMStreamSendsFrames(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-3", []string{"echo"}, nil)

	var frames []map[string]interface{}

	ch := newMockChannel()
	demux := newChannelDemuxer(ch, nil)
	inst.demuxer = demux
	defer demux.Stop()

	// Mock Ollama server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"Hello"}}]}`)
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":" world"}}]}`)
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})

	m.proxyLLMStream(inst, demux, srv.URL, "req-1", body)

	// Read sent messages from the mock channel
	ch.mu.Lock()
	for _, msg := range ch.sendBuf {
		var parsed struct {
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if json.Unmarshal(msg, &parsed) == nil && parsed.Method == "llm.frame" {
			frames = append(frames, parsed.Params)
		}
	}
	ch.mu.Unlock()

	// Expect: 2 llm.delta + 1 llm.done
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}

	// First two are deltas
	for i := 0; i < 2; i++ {
		if frames[i]["type"] != "llm.delta" {
			t.Errorf("frame[%d].type = %v, want llm.delta", i, frames[i]["type"])
		}
		if frames[i]["req_id"] != "req-1" {
			t.Errorf("frame[%d].req_id = %v, want req-1", i, frames[i]["req_id"])
		}
	}

	// Last is done
	if frames[2]["type"] != "llm.done" {
		t.Errorf("frame[2].type = %v, want llm.done", frames[2]["type"])
	}

	// Verify delta content
	if !strings.Contains(frames[0]["data"].(string), "Hello") {
		t.Errorf("frame[0].data = %v, want Hello", frames[0]["data"])
	}
	if !strings.Contains(frames[1]["data"].(string), "world") {
		t.Errorf("frame[1].data = %v, want world", frames[1]["data"])
	}
}

func TestProxyLLMStreamError(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-4", []string{"echo"}, nil)

	ch := newMockChannel()
	demux := newChannelDemuxer(ch, nil)
	inst.demuxer = demux
	defer demux.Stop()

	// Server returns 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	m.proxyLLMStream(inst, demux, srv.URL, "req-err", json.RawMessage(`{}`))

	// Should have sent an llm.error frame
	ch.mu.Lock()
	defer ch.mu.Unlock()

	var errorFrame map[string]interface{}
	for _, msg := range ch.sendBuf {
		var parsed struct {
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if json.Unmarshal(msg, &parsed) == nil && parsed.Method == "llm.frame" {
			if parsed.Params["type"] == "llm.error" {
				errorFrame = parsed.Params
			}
		}
	}

	if errorFrame == nil {
		t.Fatal("expected llm.error frame")
	}
	errMsg, _ := errorFrame["error"].(string)
	if !strings.Contains(errMsg, "500") {
		t.Errorf("error = %q, want status 500", errMsg)
	}
}

func TestProxyLLMStreamUnreachable(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-5", []string{"echo"}, nil)

	ch := newMockChannel()
	demux := newChannelDemuxer(ch, nil)
	inst.demuxer = demux
	defer demux.Stop()

	// Point to a port that's not listening
	m.proxyLLMStream(inst, demux, "http://localhost:19999/v1/chat/completions", "req-unreach", json.RawMessage(`{}`))

	ch.mu.Lock()
	defer ch.mu.Unlock()

	var gotError bool
	for _, msg := range ch.sendBuf {
		var parsed struct {
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if json.Unmarshal(msg, &parsed) == nil && parsed.Method == "llm.frame" {
			if parsed.Params["type"] == "llm.error" {
				gotError = true
			}
		}
	}

	if !gotError {
		t.Fatal("expected llm.error frame for unreachable provider")
	}
}

func TestHostLLMProviderMap(t *testing.T) {
	tests := []struct {
		provider string
		wantPort string
		wantOK   bool
	}{
		{"ollama", "11434", true},
		{"lmstudio", "1234", true},
		{"vllm", "8000", true},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			endpoint, ok := hostLLMProviders[tt.provider]
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && !strings.Contains(endpoint, tt.wantPort) {
				t.Errorf("endpoint = %q, want port %s", endpoint, tt.wantPort)
			}
		})
	}
}

func TestHandleLLMChatNilDemuxer(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-6", []string{"echo"}, nil)
	// demuxer is nil

	params, _ := json.Marshal(map[string]interface{}{
		"provider": "ollama",
		"model":    "test",
		"body":     json.RawMessage(`{}`),
	})

	_, err := m.handleLLMChat(inst, params)
	if err == nil {
		t.Fatal("expected error for nil demuxer")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error = %v, want 'not connected'", err)
	}
}

func TestProxyLLMStreamMidStreamDisconnect(t *testing.T) {
	m := newTestManager()
	inst := m.CreateInstance("test-7", []string{"echo"}, nil)

	ch := newMockChannel()
	demux := newChannelDemuxer(ch, nil)
	inst.demuxer = demux
	defer demux.Stop()

	// Server sends partial stream then closes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
		// Don't send [DONE] — just close the connection
	}))
	defer srv.Close()

	m.proxyLLMStream(inst, demux, srv.URL, "req-partial", json.RawMessage(`{}`))

	// Should get a delta frame then a done frame (stream ended naturally via EOF)
	ch.mu.Lock()
	defer ch.mu.Unlock()

	var types []string
	for _, msg := range ch.sendBuf {
		var parsed struct {
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if json.Unmarshal(msg, &parsed) == nil && parsed.Method == "llm.frame" {
			typ, _ := parsed.Params["type"].(string)
			types = append(types, typ)
		}
	}

	// Expect delta + done (scanner finishes on EOF without error)
	if len(types) < 2 {
		t.Fatalf("got %d frames (%v), want at least 2", len(types), types)
	}
	if types[0] != "llm.delta" {
		t.Errorf("frame[0] = %s, want llm.delta", types[0])
	}
	// Last frame should be llm.done (clean EOF)
	if types[len(types)-1] != "llm.done" {
		t.Errorf("last frame = %s, want llm.done", types[len(types)-1])
	}
}

// Uses newTestManager() from manager_test.go and mockChannel from demuxer_test.go.
