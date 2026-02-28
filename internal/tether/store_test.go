package tether

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestAppend_PersistCalled(t *testing.T) {
	var mu sync.Mutex
	var calls []struct {
		instanceID string
		seq        int64
		data       []byte
	}

	s := NewStore(func(instanceID string, seq int64, data []byte) {
		mu.Lock()
		calls = append(calls, struct {
			instanceID string
			seq        int64
			data       []byte
		}{instanceID, seq, data})
		mu.Unlock()
	})

	s.Append("inst-1", Frame{V: 1, Type: "user.message", Session: SessionID{Channel: "host", ID: "default"}})
	s.Append("inst-1", Frame{V: 1, Type: "assistant.done", Session: SessionID{Channel: "host", ID: "default"}})
	s.Append("inst-2", Frame{V: 1, Type: "user.message", Session: SessionID{Channel: "host", ID: "s2"}})

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 3 {
		t.Fatalf("expected 3 persist calls, got %d", len(calls))
	}

	if calls[0].instanceID != "inst-1" || calls[0].seq != 1 {
		t.Errorf("call[0]: instance=%q seq=%d, want inst-1/1", calls[0].instanceID, calls[0].seq)
	}
	if calls[1].instanceID != "inst-1" || calls[1].seq != 2 {
		t.Errorf("call[1]: instance=%q seq=%d, want inst-1/2", calls[1].instanceID, calls[1].seq)
	}
	if calls[2].instanceID != "inst-2" || calls[2].seq != 1 {
		t.Errorf("call[2]: instance=%q seq=%d, want inst-2/1", calls[2].instanceID, calls[2].seq)
	}

	// Verify persisted data is valid JSON with correct seq
	var f Frame
	if err := json.Unmarshal(calls[0].data, &f); err != nil {
		t.Fatalf("unmarshal persisted frame: %v", err)
	}
	if f.Seq != 1 || f.Type != "user.message" {
		t.Errorf("persisted frame: seq=%d type=%q, want 1/user.message", f.Seq, f.Type)
	}
}

func TestAppend_NilPersist(t *testing.T) {
	s := NewStore(nil)

	// Should not panic
	seq := s.Append("inst-1", Frame{V: 1, Type: "user.message"})
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
}

func TestLoad_RestoresFrames(t *testing.T) {
	s := NewStore(nil)

	frames := []Frame{
		{V: 1, Type: "user.message", Seq: 5, Session: SessionID{Channel: "host", ID: "default"}},
		{V: 1, Type: "assistant.done", Seq: 6, Session: SessionID{Channel: "host", ID: "default"}},
	}

	s.Load("inst-1", frames)

	result := s.Query("inst-1", QueryOpts{AfterSeq: 0, Limit: 50})
	if len(result.Frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(result.Frames))
	}
	if result.Frames[0].Seq != 5 {
		t.Errorf("frame[0].Seq = %d, want 5", result.Frames[0].Seq)
	}
	if result.Frames[1].Seq != 6 {
		t.Errorf("frame[1].Seq = %d, want 6", result.Frames[1].Seq)
	}
	if result.Frames[0].Type != "user.message" {
		t.Errorf("frame[0].Type = %q, want user.message", result.Frames[0].Type)
	}
}

func TestLoad_SeqContinuity(t *testing.T) {
	s := NewStore(nil)

	// Load frames with seq 10, 11
	s.Load("inst-1", []Frame{
		{V: 1, Type: "user.message", Seq: 10},
		{V: 1, Type: "assistant.done", Seq: 11},
	})

	// Append should continue from 12
	seq := s.Append("inst-1", Frame{V: 1, Type: "user.message"})
	if seq != 12 {
		t.Errorf("seq after load+append = %d, want 12", seq)
	}

	// All 3 frames should be queryable
	result := s.Query("inst-1", QueryOpts{AfterSeq: 0, Limit: 50})
	if len(result.Frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(result.Frames))
	}
}

func TestLoad_Empty(t *testing.T) {
	s := NewStore(nil)

	// Should not panic or create a buffer
	s.Load("inst-1", nil)
	s.Load("inst-1", []Frame{})

	result := s.Query("inst-1", QueryOpts{AfterSeq: 0})
	if len(result.Frames) != 0 {
		t.Errorf("expected 0 frames after empty load, got %d", len(result.Frames))
	}
}

func TestLoad_DoesNotCallPersist(t *testing.T) {
	called := false
	s := NewStore(func(string, int64, []byte) {
		called = true
	})

	s.Load("inst-1", []Frame{
		{V: 1, Type: "user.message", Seq: 1},
	})

	if called {
		t.Error("persist should not be called during Load")
	}
}

func TestLoad_RingBufferWraparound(t *testing.T) {
	s := NewStore(nil)

	// defaultRingSize is 1000 — load 1005 frames to trigger wraparound
	frames := make([]Frame, 1005)
	for i := range frames {
		frames[i] = Frame{V: 1, Type: "user.message", Seq: int64(i + 1)}
	}

	s.Load("inst-1", frames)

	// Should keep the last 1000
	result := s.Query("inst-1", QueryOpts{AfterSeq: 0, Limit: 200})
	if len(result.Frames) != 200 {
		t.Fatalf("expected 200 frames (limit), got %d", len(result.Frames))
	}
	// First frame in buffer should be seq 6 (frames 6-1005)
	if result.Frames[0].Seq != 6 {
		t.Errorf("first frame seq = %d, want 6", result.Frames[0].Seq)
	}

	// Seq counter should be at 1005
	seq := s.Append("inst-1", Frame{V: 1, Type: "user.message"})
	if seq != 1006 {
		t.Errorf("seq after wraparound load + append = %d, want 1006", seq)
	}
}

func TestLoad_QueryWithFilters(t *testing.T) {
	s := NewStore(nil)

	s.Load("inst-1", []Frame{
		{V: 1, Type: "user.message", Seq: 1, Session: SessionID{Channel: "host", ID: "s1"}},
		{V: 1, Type: "assistant.done", Seq: 2, Session: SessionID{Channel: "host", ID: "s1"}},
		{V: 1, Type: "user.message", Seq: 3, Session: SessionID{Channel: "telegram", ID: "s2"}},
	})

	// Filter by session
	result := s.Query("inst-1", QueryOpts{AfterSeq: 0, SessionID: "s1"})
	if len(result.Frames) != 2 {
		t.Fatalf("expected 2 frames for session s1, got %d", len(result.Frames))
	}

	// Filter by type
	result = s.Query("inst-1", QueryOpts{AfterSeq: 0, Types: []string{"assistant.done"}})
	if len(result.Frames) != 1 {
		t.Fatalf("expected 1 assistant.done frame, got %d", len(result.Frames))
	}

	// Filter by afterSeq
	result = s.Query("inst-1", QueryOpts{AfterSeq: 2})
	if len(result.Frames) != 1 {
		t.Fatalf("expected 1 frame after seq 2, got %d", len(result.Frames))
	}
	if result.Frames[0].Seq != 3 {
		t.Errorf("frame seq = %d, want 3", result.Frames[0].Seq)
	}
}

func TestLoad_MultipleInstances(t *testing.T) {
	s := NewStore(nil)

	s.Load("inst-a", []Frame{
		{V: 1, Type: "user.message", Seq: 10},
	})
	s.Load("inst-b", []Frame{
		{V: 1, Type: "assistant.done", Seq: 20},
	})

	rA := s.Query("inst-a", QueryOpts{AfterSeq: 0})
	rB := s.Query("inst-b", QueryOpts{AfterSeq: 0})

	if len(rA.Frames) != 1 || rA.Frames[0].Seq != 10 {
		t.Errorf("inst-a: expected 1 frame with seq 10, got %d frames", len(rA.Frames))
	}
	if len(rB.Frames) != 1 || rB.Frames[0].Seq != 20 {
		t.Errorf("inst-b: expected 1 frame with seq 20, got %d frames", len(rB.Frames))
	}

	// Appends should be independent
	seqA := s.Append("inst-a", Frame{V: 1, Type: "user.message"})
	seqB := s.Append("inst-b", Frame{V: 1, Type: "user.message"})
	if seqA != 11 {
		t.Errorf("inst-a next seq = %d, want 11", seqA)
	}
	if seqB != 21 {
		t.Errorf("inst-b next seq = %d, want 21", seqB)
	}
}
