package tether

import "sync"

const defaultRingSize = 1000

// Store is a per-instance pub/sub ring buffer for egress tether frames.
// Modeled after logstore.Store — append frames, subscribe for live updates,
// query recent history.
//
// Only egress frames (assistant.*, status.*, event.*) are stored.
// Ingress frames (user.*) are fire-and-forward only.
type Store struct {
	mu      sync.RWMutex
	buffers map[string]*ringBuffer
}

// NewStore creates a new tether store.
func NewStore() *Store {
	return &Store{
		buffers: make(map[string]*ringBuffer),
	}
}

// Append adds an egress frame to the instance's ring buffer and notifies subscribers.
func (s *Store) Append(instanceID string, frame Frame) {
	s.mu.Lock()
	rb, ok := s.buffers[instanceID]
	if !ok {
		rb = newRingBuffer(defaultRingSize)
		s.buffers[instanceID] = rb
	}
	s.mu.Unlock()

	rb.append(frame)
}

// Subscribe returns a channel that receives new egress frames for an instance,
// plus a cancel function to unsubscribe.
func (s *Store) Subscribe(instanceID string) (<-chan Frame, func()) {
	s.mu.Lock()
	rb, ok := s.buffers[instanceID]
	if !ok {
		rb = newRingBuffer(defaultRingSize)
		s.buffers[instanceID] = rb
	}
	s.mu.Unlock()

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

// ringBuffer is a fixed-size circular buffer with subscriber notification.
type ringBuffer struct {
	mu      sync.Mutex
	frames  []Frame
	head    int
	count   int
	cap     int
	subs    []chan Frame
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{
		frames: make([]Frame, capacity),
		cap:    capacity,
	}
}

func (rb *ringBuffer) append(frame Frame) {
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
	rb.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
			// subscriber slow — drop frame
		}
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
}
