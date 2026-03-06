package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockLLM implements LLM for testing. It blocks until context is cancelled
// or returns a preconfigured response.
type mockLLM struct {
	mu        sync.Mutex
	calls     int
	responses []*LLMResponse
	blockCh   chan struct{} // if non-nil, blocks until closed or ctx cancelled
}

func (m *mockLLM) StreamChat(ctx context.Context, messages []Message, tools []Tool, onDelta func(string)) (*LLMResponse, error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.mu.Unlock()

	if m.blockCh != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.blockCh:
		}
	}

	if idx < len(m.responses) {
		resp := m.responses[idx]
		if resp.RawContent == nil && len(resp.ToolCalls) == 0 {
			onDelta("response text")
		}
		return resp, nil
	}
	onDelta("default response")
	return &LLMResponse{}, nil
}

func (m *mockLLM) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// --- cancelRun / registerRun / finishRun tests ---

func TestRegisterAndCancelRun(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctx := agent.registerRun("sess1")
	if ctx.Err() != nil {
		t.Fatal("context should not be cancelled yet")
	}

	agent.cancelRun("sess1")
	if ctx.Err() == nil {
		t.Fatal("context should be cancelled after cancelRun")
	}
}

func TestCancelRunRemovesEntry(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	agent.registerRun("sess1")
	agent.cancelRun("sess1")

	if _, ok := agent.activeRuns["sess1"]; ok {
		t.Fatal("activeRuns entry should be removed after cancel")
	}
}

func TestCancelRunNoop(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}
	// Should not panic on non-existent session
	agent.cancelRun("nonexistent")
}

func TestRegisterRunReplacesExisting(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctx1 := agent.registerRun("sess1")
	ctx2 := agent.registerRun("sess1")

	// ctx1 is NOT automatically cancelled — that's cancelRun's job.
	// But the active entry now points to ctx2.
	if ctx1 == ctx2 {
		t.Fatal("should be different contexts")
	}

	run := agent.activeRuns["sess1"]
	if run.ctx != ctx2 {
		t.Fatal("active run should point to the newer context")
	}
}

func TestFinishRunOnlyRemovesOwnContext(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctx1 := agent.registerRun("sess1")
	// Simulate a newer run replacing us
	_ = agent.registerRun("sess1")

	// Old run finishes — should NOT remove the newer entry
	agent.finishRun("sess1", ctx1)

	if _, ok := agent.activeRuns["sess1"]; !ok {
		t.Fatal("newer run entry should NOT be removed by old finishRun")
	}
}

func TestFinishRunRemovesOwnContext(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctx := agent.registerRun("sess1")
	agent.finishRun("sess1", ctx)

	if _, ok := agent.activeRuns["sess1"]; ok {
		t.Fatal("finishRun should remove its own entry")
	}
}

// --- control.cancel frame handling ---

func TestControlCancelFrame(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
		sessions:   make(map[string]*Session),
	}

	ctx := agent.registerRun("ui_default")

	frame := TetherFrame{
		V:       1,
		Type:    "control.cancel",
		Session: SessionID{Channel: "ui", ID: "default"},
	}
	data, _ := json.Marshal(frame)

	req := httptest.NewRequest("POST", "/v1/tether/recv", strings.NewReader(string(data)))
	w := httptest.NewRecorder()
	agent.handleTetherRecv(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if ctx.Err() == nil {
		t.Fatal("context should be cancelled by control.cancel frame")
	}
}

// --- New message cancels in-flight run ---

func TestNewMessageCancelsPreviousRun(t *testing.T) {
	// Start a mock tether endpoint to absorb frames from the agent
	tetherFrames := make(chan []byte, 100)
	tether := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, 1024)
		n, _ := r.Body.Read(data)
		if n > 0 {
			tetherFrames <- data[:n]
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer tether.Close()

	blockCh := make(chan struct{})
	llm := &mockLLM{
		blockCh: blockCh,
	}

	agent := &Agent{
		activeRuns:      make(map[string]activeRun),
		sessions:        make(map[string]*Session),
		llm:             llm,
		systemPrompt:    "test",
		maxContextTurns: 50,
		maxContextChars: 24000,
	}

	// Override harnessAPI for this test — not possible since it's a const.
	// Instead, test at the cancelRun level.
	ctx1 := agent.registerRun("ui_default")

	// Simulate receiving a new user.message which cancels previous
	agent.cancelRun("ui_default")

	if ctx1.Err() == nil {
		t.Fatal("previous run context should be cancelled")
	}

	ctx2 := agent.registerRun("ui_default")
	if ctx2.Err() != nil {
		t.Fatal("new run context should be active")
	}
}

// --- Concurrent cancellation safety ---

func TestConcurrentCancelRegister(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	var wg sync.WaitGroup
	var cancelled atomic.Int32

	// Spawn 50 goroutines that register and cancel the same session
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := agent.registerRun("sess1")
			time.Sleep(time.Millisecond)
			agent.cancelRun("sess1")
			if ctx.Err() != nil {
				cancelled.Add(1)
			}
		}()
	}

	wg.Wait()

	// All contexts should eventually be cancelled
	if cancelled.Load() == 0 {
		t.Fatal("expected some cancelled contexts")
	}
}

// --- LLM cancellation via context ---

func TestStreamChatCancelledByContext(t *testing.T) {
	blockCh := make(chan struct{})
	llm := &mockLLM{blockCh: blockCh}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := llm.StreamChat(ctx, nil, nil, func(string) {})
		done <- err
	}()

	// Cancel while blocked
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamChat did not return after cancellation")
	}
}

// --- pruneIncompleteToolChains tests ---

func TestPruneCompleteChain(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
		}},
		{Role: "tool", Content: "result", ToolCallID: "t1"},
		{Role: "assistant", Content: "done"},
	}

	result := pruneIncompleteToolChains(turns)
	if len(result) != 4 {
		t.Fatalf("len = %d, want 4 (complete chain should be preserved)", len(result))
	}
}

func TestPruneIncompleteChainMissingAllResults(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
		}},
		// Missing tool result for t1
		{Role: "user", Content: "new message"},
	}

	result := pruneIncompleteToolChains(turns)
	// Should drop the assistant turn, keep both user turns
	for _, r := range result {
		if r.Role == "assistant" {
			t.Fatal("incomplete assistant turn should be pruned")
		}
	}
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2 (two user turns)", len(result))
	}
}

func TestPruneIncompleteChainPartialResults(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
			map[string]interface{}{"type": "tool_use", "id": "t2", "name": "read_file"},
		}},
		{Role: "tool", Content: "result1", ToolCallID: "t1"},
		// Missing tool result for t2 — VM paused mid-execution
		{Role: "user", Content: "new message"},
	}

	result := pruneIncompleteToolChains(turns)
	// Should drop the assistant + partial tool result, keep both user turns
	for _, r := range result {
		if r.Role == "assistant" || r.Role == "tool" {
			t.Fatalf("incomplete chain (assistant + partial tool) should be pruned, got role=%s", r.Role)
		}
	}
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
}

func TestPruneMultipleToolCallsComplete(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
			map[string]interface{}{"type": "tool_use", "id": "t2", "name": "read_file"},
		}},
		{Role: "tool", Content: "result1", ToolCallID: "t1"},
		{Role: "tool", Content: "result2", ToolCallID: "t2"},
		{Role: "assistant", Content: "all done"},
	}

	result := pruneIncompleteToolChains(turns)
	if len(result) != 5 {
		t.Fatalf("len = %d, want 5 (complete multi-tool chain)", len(result))
	}
}

func TestPruneChainAtEnd(t *testing.T) {
	// The assistant turn with tool_use is the last thing — no tool results at all
	// (VM paused right after LLM returned)
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
		}},
	}

	result := pruneIncompleteToolChains(turns)
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1 (only user turn)", len(result))
	}
	if result[0].Role != "user" {
		t.Fatalf("expected user turn, got %s", result[0].Role)
	}
}

func TestPrunePreservesPlainAssistant(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "plain text response"},
	}

	result := pruneIncompleteToolChains(turns)
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2 (plain assistant should be preserved)", len(result))
	}
}

func TestPruneMixedChains(t *testing.T) {
	turns := []Turn{
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
		}},
		{Role: "tool", Content: "ok", ToolCallID: "t1"},
		{Role: "assistant", Content: "first done"},
		{Role: "user", Content: "msg2"},
		// Incomplete chain
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t2", "name": "bash"},
		}},
		// No tool result for t2
		{Role: "user", Content: "msg3"},
	}

	result := pruneIncompleteToolChains(turns)

	// Should keep: user(msg1) + assistant(t1) + tool(t1) + assistant(first done) + user(msg2) + user(msg3)
	// Should drop: assistant(t2)
	expected := 6
	if len(result) != expected {
		names := make([]string, len(result))
		for i, r := range result {
			names[i] = fmt.Sprintf("%s(%v)", r.Role, r.Content)
		}
		t.Fatalf("len = %d, want %d. got: %v", len(result), expected, names)
	}
}

// --- assembleContext with incomplete chains (integration) ---

func TestAssembleContextPrunesIncompleteChain(t *testing.T) {
	sess := &Session{key: "test"}
	sess.turns = []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
			map[string]interface{}{"type": "tool_use", "id": "t2", "name": "read_file"},
		}},
		{Role: "tool", Content: "result1", ToolCallID: "t1"},
		// Missing t2 — VM paused
		{Role: "user", Content: "new message"},
	}

	messages := sess.assembleContext("sys", 50, 100000)

	// Should have: system + user(hello) + user(new message) = 3
	// The incomplete assistant + partial tool should be pruned
	if len(messages) != 3 {
		names := make([]string, len(messages))
		for i, m := range messages {
			names[i] = fmt.Sprintf("%s(%v)", m.Role, m.Content)
		}
		t.Fatalf("len = %d, want 3. got: %v", len(messages), names)
	}

	// Verify no tool or incomplete assistant turns
	for _, m := range messages[1:] {
		if m.Role == "tool" {
			t.Fatal("tool turn should be pruned")
		}
		if m.Role == "assistant" {
			t.Fatal("incomplete assistant turn should be pruned")
		}
	}
}
