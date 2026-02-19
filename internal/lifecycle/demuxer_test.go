package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockChannel is a minimal ControlChannel for testing the demuxer.
type mockChannel struct {
	mu       sync.Mutex
	recvCh   chan []byte    // test feeds messages here
	sendBuf  [][]byte      // captures sent messages
	closed   bool
	closedCh chan struct{} // closed when Close() is called
}

func newMockChannel() *mockChannel {
	return &mockChannel{
		recvCh:   make(chan []byte, 10),
		closedCh: make(chan struct{}),
	}
}

func (m *mockChannel) Send(ctx context.Context, msg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return context.Canceled
	}
	cp := make([]byte, len(msg))
	copy(cp, msg)
	m.sendBuf = append(m.sendBuf, cp)
	return nil
}

func (m *mockChannel) Recv(ctx context.Context) ([]byte, error) {
	select {
	case msg, ok := <-m.recvCh:
		if !ok {
			return nil, context.Canceled
		}
		return msg, nil
	case <-m.closedCh:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *mockChannel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.closedCh)
	}
	return nil
}

func (m *mockChannel) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sendBuf)
}

// --- Demuxer tests ---

func TestDemuxerStopReturnsPromptly(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)

	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("demuxer.Stop() did not return within 2 seconds")
	}
}

func TestDemuxerCallResponse(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, func(method string, params json.RawMessage) {})
	defer d.Stop()

	go func() {
		time.Sleep(50 * time.Millisecond)
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"result":  map[string]string{"status": "ok"},
			"id":      1,
		})
		ch.recvCh <- resp
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := d.Call(ctx, "health", nil, 1)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
}

func TestDemuxerNotificationRouting(t *testing.T) {
	ch := newMockChannel()

	var gotMethod string
	var gotParams json.RawMessage
	d := newChannelDemuxer(ch, func(method string, params json.RawMessage) {
		gotMethod = method
		gotParams = params
	})
	defer d.Stop()

	notif, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "log",
		"params":  map[string]string{"stream": "stdout", "line": "hello"},
	})
	ch.recvCh <- notif
	time.Sleep(100 * time.Millisecond)

	if gotMethod != "log" {
		t.Fatalf("expected method 'log', got %q", gotMethod)
	}
	if gotParams == nil {
		t.Fatal("expected params, got nil")
	}
}

func TestDemuxerCallTimeout(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)
	defer d.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := d.Call(ctx, "health", nil, 1)
	if err == nil {
		t.Fatal("expected error on timeout, got nil")
	}
}

func TestDemuxerStopDuringCall(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := d.Call(ctx, "slowMethod", nil, 1)
		errCh <- err
	}()

	// Let Call register and block
	time.Sleep(50 * time.Millisecond)

	// Stop while Call is in flight
	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung while Call was in flight")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected Call to return error after Stop, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after Stop")
	}
}

func TestDemuxerConcurrentCalls(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)
	defer d.Stop()

	const numCalls = 5
	var wg sync.WaitGroup

	// Feed responses for all calls
	go func() {
		time.Sleep(50 * time.Millisecond)
		for i := 1; i <= numCalls; i++ {
			resp, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  map[string]string{"call": fmt.Sprintf("%d", i)},
				"id":      i,
			})
			ch.recvCh <- resp
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var successes int64
	for i := 1; i <= numCalls; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := d.Call(ctx, "test", nil, id)
			if err == nil {
				atomic.AddInt64(&successes, 1)
			}
		}(i)
	}

	wg.Wait()
	if successes != numCalls {
		t.Fatalf("expected %d successful calls, got %d", numCalls, successes)
	}
}

func TestDemuxerChannelErrorCleansPending(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := d.Call(ctx, "test", nil, 1)
		errCh <- err
	}()

	// Let Call register
	time.Sleep(50 * time.Millisecond)

	// Simulate channel error by closing the recv channel
	close(ch.recvCh)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after channel close, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after channel error")
	}

	// Stop should still work cleanly
	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung after channel error")
	}
}

func TestDemuxerDoneClosedOnStop(t *testing.T) {
	ch := newMockChannel()
	d := newChannelDemuxer(ch, nil)

	select {
	case <-d.Done():
		t.Fatal("Done() should not be closed before Stop()")
	default:
	}

	d.Stop()

	select {
	case <-d.Done():
		// ok
	case <-time.After(time.Second):
		t.Fatal("Done() not closed after Stop()")
	}
}

// --- Exec waiter tests ---

func TestExecWaiterSignaledOnDone(t *testing.T) {
	inst := &Instance{
		execWaiters: make(map[string]chan int),
	}

	doneCh := make(chan int, 1)
	inst.execWaiters["exec-1"] = doneCh

	// Simulate what onNotif("execDone") does
	inst.mu.Lock()
	if ch, ok := inst.execWaiters["exec-1"]; ok {
		ch <- 0
		close(ch)
		delete(inst.execWaiters, "exec-1")
	}
	inst.mu.Unlock()

	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("done channel not signaled")
	}
}

func TestExecWaiterSignaledOnInstanceStop(t *testing.T) {
	inst := &Instance{
		execWaiters: make(map[string]chan int),
	}

	done1 := make(chan int, 1)
	done2 := make(chan int, 1)
	inst.execWaiters["exec-1"] = done1
	inst.execWaiters["exec-2"] = done2

	// Simulate what StopInstance does
	inst.mu.Lock()
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}
	inst.mu.Unlock()

	for name, ch := range map[string]chan int{"exec-1": done1, "exec-2": done2} {
		select {
		case code := <-ch:
			if code != -1 {
				t.Fatalf("%s: expected exit code -1, got %d", name, code)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: done channel not signaled", name)
		}
	}

	if len(inst.execWaiters) != 0 {
		t.Fatalf("expected empty execWaiters, got %d", len(inst.execWaiters))
	}
}

func TestExecWaiterNotBlockedByMissingExecID(t *testing.T) {
	inst := &Instance{
		execWaiters: make(map[string]chan int),
	}

	// Signal for an exec ID that has no waiter — should not panic
	inst.mu.Lock()
	if ch, ok := inst.execWaiters["nonexistent"]; ok {
		ch <- 0
		close(ch)
	}
	inst.mu.Unlock()
	// If we get here without panic, test passes
}
