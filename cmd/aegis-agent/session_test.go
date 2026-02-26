package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestAssembleContextRespectsTurnLimit(t *testing.T) {
	sess := &Session{key: "test"}

	// Add 10 turns
	for i := 0; i < 10; i++ {
		sess.turns = append(sess.turns, Turn{
			Role:    "user",
			Content: fmt.Sprintf("message %d", i),
			TS:      now(),
		})
	}

	// Limit to 5 turns
	messages := sess.assembleContext("system prompt", 5, 100000)

	// 1 system + 5 turns = 6
	if len(messages) != 6 {
		t.Errorf("len(messages) = %d, want 6 (1 system + 5 turns)", len(messages))
	}

	// Should include the last 5 turns
	if msg, ok := messages[1].Content.(string); ok {
		if msg != "message 5" {
			t.Errorf("first included turn = %q, want 'message 5'", msg)
		}
	}
}

func TestAssembleContextRespectsCharLimit(t *testing.T) {
	sess := &Session{key: "test"}

	// Add turns with known sizes
	for i := 0; i < 10; i++ {
		// Each message is ~20 chars
		sess.turns = append(sess.turns, Turn{
			Role:    "user",
			Content: fmt.Sprintf("msg-%02d-padding-text", i),
			TS:      now(),
		})
	}

	// Limit to 50 chars — should fit ~2 turns
	messages := sess.assembleContext("sys", 100, 50)

	// Should have system + some turns (at most a few)
	turnCount := len(messages) - 1 // subtract system message
	if turnCount > 3 {
		t.Errorf("too many turns included with 50 char limit: %d", turnCount)
	}
	if turnCount < 1 {
		t.Errorf("no turns included with 50 char limit")
	}
}

func TestAssembleContextSystemPrompt(t *testing.T) {
	sess := &Session{key: "test"}
	sess.turns = append(sess.turns, Turn{Role: "user", Content: "hello"})

	messages := sess.assembleContext("custom system prompt", 50, 24000)

	if len(messages) < 1 {
		t.Fatal("no messages returned")
	}
	if messages[0].Role != "system" {
		t.Errorf("first message role = %q, want 'system'", messages[0].Role)
	}
	if messages[0].Content != "custom system prompt" {
		t.Errorf("system prompt = %q, want 'custom system prompt'", messages[0].Content)
	}
}

func TestAssembleContextToolChainIntegrity(t *testing.T) {
	sess := &Session{key: "test"}

	// Create a valid tool chain: user → assistant(tool_use) → tool → assistant
	sess.turns = append(sess.turns,
		Turn{Role: "user", Content: "do something"},
		Turn{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash", "input": "{}"},
		}},
		Turn{Role: "tool", Content: "result", ToolCallID: "t1"},
		Turn{Role: "assistant", Content: "done"},
	)

	messages := sess.assembleContext("sys", 50, 100000)

	// All turns should be included
	if len(messages) != 5 { // system + 4 turns
		t.Errorf("len(messages) = %d, want 5", len(messages))
	}
}

func TestAssembleContextTrimsOrphanedToolTurns(t *testing.T) {
	sess := &Session{key: "test"}

	// Start with an orphaned tool turn (no preceding assistant+tool_use)
	sess.turns = append(sess.turns,
		Turn{Role: "tool", Content: "orphaned result", ToolCallID: "t0"},
		Turn{Role: "user", Content: "hello"},
		Turn{Role: "assistant", Content: "world"},
	)

	messages := sess.assembleContext("sys", 50, 100000)

	// The orphaned tool turn should be trimmed
	for _, m := range messages {
		if m.Role == "tool" {
			t.Error("orphaned tool turn should have been trimmed")
		}
	}
	// Should have: system + user + assistant = 3
	if len(messages) != 3 {
		t.Errorf("len(messages) = %d, want 3", len(messages))
	}
}

func TestAssembleContextTrimsOrphanedAssistantToolUse(t *testing.T) {
	sess := &Session{key: "test"}

	// Start with an assistant turn with tool_use but no following tool results
	// (this can happen when context window truncates mid-chain)
	sess.turns = append(sess.turns,
		Turn{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash"},
		}},
		Turn{Role: "tool", Content: "result", ToolCallID: "t1"},
		Turn{Role: "user", Content: "hello"},
		Turn{Role: "assistant", Content: "hi"},
	)

	messages := sess.assembleContext("sys", 50, 100000)

	// The orphaned assistant+tool chain at the start should be trimmed
	// Should have: system + user + assistant = 3
	if len(messages) != 3 {
		t.Errorf("len(messages) = %d, want 3", len(messages))
	}
	// First non-system message should be the user turn
	if messages[1].Role != "user" {
		t.Errorf("first non-system message role = %q, want 'user'", messages[1].Role)
	}
}

func TestAssembleContextEmpty(t *testing.T) {
	sess := &Session{key: "test"}

	messages := sess.assembleContext("sys", 50, 24000)

	// Just the system message
	if len(messages) != 1 {
		t.Errorf("len(messages) = %d, want 1 (system only)", len(messages))
	}
}

func TestTurnSize(t *testing.T) {
	// String content
	turn := Turn{Content: "hello world"}
	if s := turnSize(turn); s != 11 {
		t.Errorf("turnSize string = %d, want 11", s)
	}

	// Large string
	turn = Turn{Content: strings.Repeat("x", 1000)}
	if s := turnSize(turn); s != 1000 {
		t.Errorf("turnSize large string = %d, want 1000", s)
	}
}
