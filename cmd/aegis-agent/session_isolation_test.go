package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Session per-run state isolation ---

func TestSessionPendingImagesIsolated(t *testing.T) {
	sessA := &Session{key: "telegram_111"}
	sessB := &Session{key: "telegram_222"}

	sessA.pendingImages = append(sessA.pendingImages, ImageRef{
		MediaType: "image/png", Blob: "blob-a", Size: 100,
	})

	if len(sessB.pendingImages) != 0 {
		t.Fatal("sessB should have no pending images")
	}
	if len(sessA.pendingImages) != 1 {
		t.Fatal("sessA should have 1 pending image")
	}
}

func TestSessionRestartPendingIsolated(t *testing.T) {
	sessA := &Session{key: "telegram_111"}
	sessB := &Session{key: "telegram_222"}

	sessA.restartPending = true

	if sessB.restartPending {
		t.Fatal("sessB.restartPending should be false")
	}
}

func TestSessionRunStateResetBetweenRuns(t *testing.T) {
	sess := &Session{key: "ui_default"}

	// Simulate leftover state from a previous run
	sess.pendingImages = []ImageRef{{Blob: "old"}}
	sess.restartPending = true

	// Reset as handleUserMessage does
	sess.pendingImages = nil
	sess.restartPending = false

	if len(sess.pendingImages) != 0 {
		t.Fatal("pendingImages should be reset")
	}
	if sess.restartPending {
		t.Fatal("restartPending should be reset")
	}
}

// --- Parallel session independence (integration) ---

func TestParallelSessionsIndependentTurns(t *testing.T) {
	agent := &Agent{
		sessions:   make(map[string]*Session),
		activeRuns: make(map[string]activeRun),
	}

	sessA := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "111"})
	sessB := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "222"})

	sessA.appendTurn(Turn{Role: "user", Content: "hello from A"})
	sessB.appendTurn(Turn{Role: "user", Content: "hello from B"})
	sessA.appendTurn(Turn{Role: "assistant", Content: "response to A"})

	msgsA := sessA.assembleContext("sys", 50, 100000)
	msgsB := sessB.assembleContext("sys", 50, 100000)

	// sessA: system + user + assistant = 3
	if len(msgsA) != 3 {
		t.Fatalf("sessA messages = %d, want 3", len(msgsA))
	}
	// sessB: system + user = 2
	if len(msgsB) != 2 {
		t.Fatalf("sessB messages = %d, want 2", len(msgsB))
	}
}

func TestParallelSessionsIndependentCancellation(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctxA, doneA := agent.registerRun("telegram_111")
	ctxB, _ := agent.registerRun("telegram_222")

	// Cancel session A
	close(doneA)
	agent.cancelRun("telegram_111")

	if ctxA.Err() == nil {
		t.Fatal("session A should be cancelled")
	}
	if ctxB.Err() != nil {
		t.Fatal("session B should NOT be cancelled")
	}
}

// TestParallelToolExecutionNoStateBleed runs two sessions concurrently
// with tool calls that write to session-scoped state (pendingImages).
// Verifies no cross-contamination.
func TestParallelToolExecutionNoStateBleed(t *testing.T) {
	agent := &Agent{
		sessions:   make(map[string]*Session),
		activeRuns: make(map[string]activeRun),
	}

	sessA := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "111"})
	sessB := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "222"})

	var wg sync.WaitGroup

	// Simulate parallel tool execution writing pendingImages
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			sessA.pendingImages = append(sessA.pendingImages, ImageRef{
				Blob: "a-img",
			})
			time.Sleep(time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			sessB.pendingImages = append(sessB.pendingImages, ImageRef{
				Blob: "b-img",
			})
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	if len(sessA.pendingImages) != 10 {
		t.Fatalf("sessA pendingImages = %d, want 10", len(sessA.pendingImages))
	}
	if len(sessB.pendingImages) != 5 {
		t.Fatalf("sessB pendingImages = %d, want 5", len(sessB.pendingImages))
	}

	// Verify no cross-contamination
	for _, img := range sessA.pendingImages {
		if img.Blob != "a-img" {
			t.Fatalf("sessA has foreign image: %s", img.Blob)
		}
	}
	for _, img := range sessB.pendingImages {
		if img.Blob != "b-img" {
			t.Fatalf("sessB has foreign image: %s", img.Blob)
		}
	}
}

// TestParallelSessionsConcurrentToolLoops simulates two full agent-like loops
// running concurrently on different sessions with a shared mock LLM.
func TestParallelSessionsConcurrentToolLoops(t *testing.T) {
	llm := &mockLLM{
		responses: []*LLMResponse{
			// First call: return a tool call
			{
				ToolCalls: []ToolCall{
					{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"echo test"}`)},
				},
				RawContent: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "t1", "name": "bash", "input": json.RawMessage(`{}`)},
				},
			},
			// Second call: return plain text (no tools)
			{},
			// Third call (for session B): tool call
			{
				ToolCalls: []ToolCall{
					{ID: "t2", Name: "bash", Input: json.RawMessage(`{"command":"echo test2"}`)},
				},
				RawContent: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "t2", "name": "bash", "input": json.RawMessage(`{}`)},
				},
			},
			// Fourth call: plain text
			{},
		},
	}

	agent := &Agent{
		sessions:        make(map[string]*Session),
		activeRuns:      make(map[string]activeRun),
		llm:             llm,
		systemPrompt:    "test",
		maxContextTurns: 50,
		maxContextChars: 24000,
	}

	sessA := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "111"})
	sessB := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "222"})

	var completedA, completedB atomic.Bool
	var wg sync.WaitGroup

	// Simulate two agent loops reading from LLM and writing to their own sessions
	wg.Add(2)
	go func() {
		defer wg.Done()
		sessA.appendTurn(Turn{Role: "user", Content: "task for A"})
		sessA.pendingImages = nil
		sessA.restartPending = false
		completedA.Store(true)
	}()
	go func() {
		defer wg.Done()
		sessB.appendTurn(Turn{Role: "user", Content: "task for B"})
		sessB.pendingImages = nil
		sessB.restartPending = false
		completedB.Store(true)
	}()
	wg.Wait()

	if !completedA.Load() || !completedB.Load() {
		t.Fatal("both sessions should complete")
	}

	// Verify turns are independent
	sessA.mu.Lock()
	aLen := len(sessA.turns)
	sessA.mu.Unlock()
	sessB.mu.Lock()
	bLen := len(sessB.turns)
	sessB.mu.Unlock()

	if aLen != 1 {
		t.Fatalf("sessA turns = %d, want 1", aLen)
	}
	if bLen != 1 {
		t.Fatalf("sessB turns = %d, want 1", bLen)
	}

	if sessA.turns[0].Content != "task for A" {
		t.Fatalf("sessA content = %v, want 'task for A'", sessA.turns[0].Content)
	}
	if sessB.turns[0].Content != "task for B" {
		t.Fatalf("sessB content = %v, want 'task for B'", sessB.turns[0].Content)
	}
}

// TestSameChannelDifferentIDsAreIndependent verifies that telegram_111 and
// telegram_222 are separate sessions even though they share a channel.
func TestSameChannelDifferentIDsAreIndependent(t *testing.T) {
	agent := &Agent{
		sessions:   make(map[string]*Session),
		activeRuns: make(map[string]activeRun),
	}

	s1 := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "111"})
	s2 := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "222"})
	s3 := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "111"}) // same as s1

	if s1 == s2 {
		t.Fatal("different IDs should return different sessions")
	}
	if s1 != s3 {
		t.Fatal("same channel+ID should return same session")
	}
}

// TestDifferentChannelsSameIDAreIndependent verifies that ui_default and
// telegram_default are separate sessions.
func TestDifferentChannelsSameIDAreIndependent(t *testing.T) {
	agent := &Agent{
		sessions:   make(map[string]*Session),
		activeRuns: make(map[string]activeRun),
	}

	s1 := agent.getOrCreateSession(SessionID{Channel: "ui", ID: "default"})
	s2 := agent.getOrCreateSession(SessionID{Channel: "telegram", ID: "default"})

	if s1 == s2 {
		t.Fatal("different channels should return different sessions")
	}

	s1.appendTurn(Turn{Role: "user", Content: "ui message"})

	s2.mu.Lock()
	s2Len := len(s2.turns)
	s2.mu.Unlock()

	if s2Len != 0 {
		t.Fatal("telegram session should have no turns after ui message")
	}
}

// TestCancelOneSessionDoesNotAffectAnother verifies that cancelling a run
// on telegram_111 does not affect telegram_222 or ui_default.
func TestCancelOneSessionDoesNotAffectAnother(t *testing.T) {
	agent := &Agent{
		activeRuns: make(map[string]activeRun),
	}

	ctx1, done1 := agent.registerRun("telegram_111")
	ctx2, _ := agent.registerRun("telegram_222")
	ctx3, _ := agent.registerRun("ui_default")

	close(done1)
	agent.cancelRun("telegram_111")

	if ctx1.Err() == nil {
		t.Fatal("telegram_111 should be cancelled")
	}
	if ctx2.Err() != nil {
		t.Fatal("telegram_222 should NOT be cancelled")
	}
	if ctx3.Err() != nil {
		t.Fatal("ui_default should NOT be cancelled")
	}
}

// TestHighConcurrencyMultipleSessions stress-tests many sessions running
// simultaneously to catch any mutex or map corruption issues.
func TestHighConcurrencyMultipleSessions(t *testing.T) {
	agent := &Agent{
		sessions:   make(map[string]*Session),
		activeRuns: make(map[string]activeRun),
	}

	const numSessions = 20
	const turnsPerSession = 50
	var wg sync.WaitGroup

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sid := SessionID{Channel: "telegram", ID: fmt.Sprintf("%d", id)}
			sess := agent.getOrCreateSession(sid)

			// Register and use a run
			sessKey := sid.Channel + "_" + sid.ID
			ctx, done := agent.registerRun(sessKey)
			defer close(done)
			defer agent.finishRun(sessKey, ctx)

			// Write turns
			for j := 0; j < turnsPerSession; j++ {
				sess.appendTurn(Turn{
					Role:    "user",
					Content: fmt.Sprintf("session %d turn %d", id, j),
				})
			}

			// Write pending images
			sess.pendingImages = append(sess.pendingImages, ImageRef{
				Blob: fmt.Sprintf("img-%d", id),
			})
		}(i)
	}
	wg.Wait()

	// Verify each session has exactly the right number of turns
	for i := 0; i < numSessions; i++ {
		sid := SessionID{Channel: "telegram", ID: fmt.Sprintf("%d", i)}
		sess := agent.getOrCreateSession(sid)

		sess.mu.Lock()
		turnCount := len(sess.turns)
		sess.mu.Unlock()

		if turnCount != turnsPerSession {
			t.Errorf("session %d: turns = %d, want %d", i, turnCount, turnsPerSession)
		}

		if len(sess.pendingImages) != 1 {
			t.Errorf("session %d: pendingImages = %d, want 1", i, len(sess.pendingImages))
		}
		if sess.pendingImages[0].Blob != fmt.Sprintf("img-%d", i) {
			t.Errorf("session %d: wrong image blob: %s", i, sess.pendingImages[0].Blob)
		}
	}

	// Verify no extra sessions leaked
	agent.mu.Lock()
	sessionCount := len(agent.sessions)
	agent.mu.Unlock()
	if sessionCount != numSessions {
		t.Errorf("session count = %d, want %d", sessionCount, numSessions)
	}
}
