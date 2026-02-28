package tether

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

const defaultRingSize = 1000

// Store is a per-instance pub/sub ring buffer for tether frames.
// Both ingress (user.*) and egress (assistant.*, status.*, event.*)
// frames are stored and queryable, giving all clients the full
// conversation history.
type Store struct {
	mu      sync.RWMutex
	buffers map[string]*ringBuffer
}

// QueryOpts filters frames in Query and WaitForFrames.
type QueryOpts struct {
	Channel      string   // filter by session.channel (e.g. "host")
	SessionID    string   // filter by session.id
	AfterSeq     int64    // frames with seq > this
	Types        []string // filter by frame type (empty = all)
	ReplyToMsgID string   // filter by reply_to msg_id
	Limit        int      // max results (0 = default 50)
}

// QueryResult is returned by Query and WaitForFrames.
type QueryResult struct {
	Frames   []Frame `json:"frames"`
	NextSeq  int64   `json:"next_seq"`
	TimedOut bool    `json:"timed_out"`
}

// MarshalJSON ensures Frames is serialized as [] not null when empty.
func (r QueryResult) MarshalJSON() ([]byte, error) {
	type Alias QueryResult
	a := Alias(r)
	if a.Frames == nil {
		a.Frames = []Frame{}
	}
	return json.Marshal(a)
}

// NewStore creates a new tether store.
func NewStore() *Store {
	return &Store{
		buffers: make(map[string]*ringBuffer),
	}
}

// Append adds a frame to the instance's ring buffer, assigns a seq,
// and notifies subscribers and poll waiters. Returns the assigned seq.
func (s *Store) Append(instanceID string, frame Frame) int64 {
	rb := s.getOrCreate(instanceID)
	return rb.append(frame)
}

// NextSeq bumps the seq counter for an instance and returns the assigned seq.
// Used for ingress frames (not stored, but need a seq for cursor tracking).
func (s *Store) NextSeq(instanceID string) int64 {
	rb := s.getOrCreate(instanceID)
	return atomic.AddInt64(&rb.seqCounter, 1)
}

// Query returns frames matching the filter criteria. Non-blocking.
func (s *Store) Query(instanceID string, opts QueryOpts) QueryResult {
	s.mu.RLock()
	rb, ok := s.buffers[instanceID]
	s.mu.RUnlock()
	if !ok {
		return QueryResult{NextSeq: opts.AfterSeq}
	}
	return rb.query(opts)
}

// WaitForFrames returns matching frames, blocking up to timeout if none available.
// Event-driven: wakes on any new frame appended to the instance, re-queries with filters.
func (s *Store) WaitForFrames(ctx context.Context, instanceID string, opts QueryOpts, timeout time.Duration) QueryResult {
	rb := s.getOrCreate(instanceID)

	// Try immediate query first
	result := rb.query(opts)
	if len(result.Frames) > 0 || timeout <= 0 {
		return result
	}

	// Long-poll: wait for new frames or timeout
	deadline := time.After(timeout)
	for {
		// Subscribe to wakeup
		wake := rb.waitChan()

		select {
		case <-ctx.Done():
			return QueryResult{NextSeq: opts.AfterSeq, TimedOut: true}
		case <-deadline:
			return QueryResult{NextSeq: opts.AfterSeq, TimedOut: true}
		case <-wake:
			result = rb.query(opts)
			if len(result.Frames) > 0 {
				return result
			}
			// Woke but no matching frames (different channel/session) — loop
		}
	}
}

// Subscribe returns a channel that receives new egress frames for an instance,
// plus a cancel function to unsubscribe.
func (s *Store) Subscribe(instanceID string) (<-chan Frame, func()) {
	rb := s.getOrCreate(instanceID)
	return rb.subscribe()
}

// Recent returns the last n egress frames for an instance.
func (s *Store) Recent(instanceID string, n int) []Frame {
	s.mu.RLock()
	rb, ok := s.buffers[instanceID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return rb.recent(n)
}

// Remove clears the buffer for an instance.
func (s *Store) Remove(instanceID string) {
	s.mu.Lock()
	if rb, ok := s.buffers[instanceID]; ok {
		rb.closeAll()
		delete(s.buffers, instanceID)
	}
	s.mu.Unlock()
}

func (s *Store) getOrCreate(instanceID string) *ringBuffer {
	s.mu.RLock()
	rb, ok := s.buffers[instanceID]
	s.mu.RUnlock()
	if ok {
		return rb
	}

	s.mu.Lock()
	rb, ok = s.buffers[instanceID]
	if !ok {
		rb = newRingBuffer(defaultRingSize)
		s.buffers[instanceID] = rb
	}
	s.mu.Unlock()
	return rb
}

// ringBuffer is a fixed-size circular buffer with subscriber notification and long-poll support.
type ringBuffer struct {
	mu         sync.Mutex
	frames     []Frame
	head       int
	count      int
	cap        int
	seqCounter int64 // atomic, per-instance monotonic
	subs       []chan Frame

	// Long-poll notification: closed and replaced on each append
	wakeCh chan struct{}
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{
		frames: make([]Frame, capacity),
		cap:    capacity,
		wakeCh: make(chan struct{}),
	}
}

func (rb *ringBuffer) append(frame Frame) int64 {
	seq := atomic.AddInt64(&rb.seqCounter, 1)
	frame.Seq = seq

	rb.mu.Lock()

	// Write to ring
	if rb.count >= rb.cap {
		rb.head = (rb.head + 1) % rb.cap
	} else {
		rb.count++
	}
	idx := (rb.head + rb.count - 1) % rb.cap
	rb.frames[idx] = frame

	// Copy subs to notify outside lock
	subs := make([]chan Frame, len(rb.subs))
	copy(subs, rb.subs)

	// Wake poll waiters: close current channel, create new one
	oldWake := rb.wakeCh
	rb.wakeCh = make(chan struct{})
	rb.mu.Unlock()

	close(oldWake)

	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
		}
	}

	return seq
}

// waitChan returns a channel that will be closed when the next frame is appended.
func (rb *ringBuffer) waitChan() <-chan struct{} {
	rb.mu.Lock()
	ch := rb.wakeCh
	rb.mu.Unlock()
	return ch
}

func (rb *ringBuffer) query(opts QueryOpts) QueryResult {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var matched []Frame
	maxSeq := opts.AfterSeq

	typesSet := make(map[string]bool, len(opts.Types))
	for _, t := range opts.Types {
		typesSet[t] = true
	}

	for i := 0; i < rb.count && len(matched) < limit; i++ {
		idx := (rb.head + i) % rb.cap
		f := rb.frames[idx]

		if f.Seq <= opts.AfterSeq {
			continue
		}
		if opts.Channel != "" && f.Session.Channel != opts.Channel {
			continue
		}
		if opts.SessionID != "" && f.Session.ID != opts.SessionID {
			continue
		}
		if len(typesSet) > 0 && !typesSet[f.Type] {
			continue
		}
		if opts.ReplyToMsgID != "" && f.MsgID != opts.ReplyToMsgID {
			continue
		}

		matched = append(matched, f)
		if f.Seq > maxSeq {
			maxSeq = f.Seq
		}
	}

	return QueryResult{
		Frames:  matched,
		NextSeq: maxSeq,
	}
}

func (rb *ringBuffer) subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, 100)

	rb.mu.Lock()
	rb.subs = append(rb.subs, ch)
	rb.mu.Unlock()

	unsub := func() {
		rb.mu.Lock()
		defer rb.mu.Unlock()
		for i, s := range rb.subs {
			if s == ch {
				rb.subs = append(rb.subs[:i], rb.subs[i+1:]...)
				break
			}
		}
		close(ch)
	}

	return ch, unsub
}

func (rb *ringBuffer) recent(n int) []Frame {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if n <= 0 || n > rb.count {
		n = rb.count
	}

	result := make([]Frame, n)
	start := rb.count - n
	for i := 0; i < n; i++ {
		idx := (rb.head + start + i) % rb.cap
		result[i] = rb.frames[idx]
	}
	return result
}

func (rb *ringBuffer) closeAll() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, ch := range rb.subs {
		close(ch)
	}
	rb.subs = nil
	close(rb.wakeCh)
}
