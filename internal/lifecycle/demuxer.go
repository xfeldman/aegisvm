package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/xfeldman/aegisvm/internal/vmm"
)

// channelDemuxer runs a persistent Recv loop on a ControlChannel,
// routing RPC responses to waiting callers and notifications to an onNotif callback.
type channelDemuxer struct {
	ch             vmm.ControlChannel
	mu             sync.Mutex // protects pending map AND serializes Send calls
	pending        map[interface{}]chan json.RawMessage
	onNotif        func(method string, params json.RawMessage)
	onGuestRequest func(method string, params json.RawMessage) (interface{}, error) // handles requests FROM harness (guest API)
	done           chan struct{}
	cancel         context.CancelFunc
}

// rpcMessage is used for parsing incoming messages to determine their type.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// newChannelDemuxer creates a demuxer and starts its Recv goroutine immediately.
func newChannelDemuxer(ch vmm.ControlChannel, onNotif func(method string, params json.RawMessage)) *channelDemuxer {
	ctx, cancel := context.WithCancel(context.Background())
	d := &channelDemuxer{
		ch:      ch,
		pending: make(map[interface{}]chan json.RawMessage),
		onNotif: onNotif,
		done:    make(chan struct{}),
		cancel:  cancel,
	}
	go d.recvLoop(ctx)
	return d
}

func (d *channelDemuxer) recvLoop(ctx context.Context) {
	defer close(d.done)
	for {
		msg, err := d.ch.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Normal shutdown (context cancelled)
				return
			}
			// Channel closed by Stop() — also normal shutdown
			if strings.Contains(err.Error(), "closed") {
				return
			}
			log.Printf("demuxer: recv error: %v", err)
			d.mu.Lock()
			for id, ch := range d.pending {
				close(ch) // signal error to waiters
				delete(d.pending, id)
			}
			d.mu.Unlock()
			return
		}

		var parsed rpcMessage
		if err := json.Unmarshal(msg, &parsed); err != nil {
			log.Printf("demuxer: invalid JSON: %v", err)
			continue
		}

		if parsed.ID != nil && parsed.Method == "" {
			// RPC response — route to pending caller
			normalizedID := normalizeID(parsed.ID)
			d.mu.Lock()
			ch, ok := d.pending[normalizedID]
			if ok {
				delete(d.pending, normalizedID)
			}
			d.mu.Unlock()
			if ok {
				ch <- msg
			} else {
				log.Printf("demuxer: no pending call for id=%v", parsed.ID)
			}
		} else if parsed.Method != "" && parsed.ID != nil {
			// RPC request FROM harness (guest API) — handle and send response
			if d.onGuestRequest != nil {
				go d.handleGuestRequest(ctx, parsed)
			} else {
				log.Printf("demuxer: guest request %s but no handler registered", parsed.Method)
			}
		} else if parsed.Method != "" && parsed.ID == nil {
			// Notification
			if d.onNotif != nil {
				d.onNotif(parsed.Method, parsed.Params)
			}
		} else {
			log.Printf("demuxer: unclassified message: %s", string(msg))
		}
	}
}

// handleGuestRequest processes an RPC request from the harness (guest API),
// calls the registered handler, and sends the response back.
func (d *channelDemuxer) handleGuestRequest(ctx context.Context, msg rpcMessage) {
	result, err := d.onGuestRequest(msg.Method, msg.Params)

	var resp []byte
	if err != nil {
		resp, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"error": map[string]interface{}{
				"code":    -32000,
				"message": err.Error(),
			},
		})
	} else {
		resp, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result":  result,
		})
	}

	d.mu.Lock()
	sendErr := d.ch.Send(ctx, resp)
	d.mu.Unlock()
	if sendErr != nil {
		log.Printf("demuxer: failed to send guest request response: %v", sendErr)
	}
}

// Call sends an RPC request and waits for the response.
func (d *channelDemuxer) Call(ctx context.Context, method string, params interface{}, id interface{}) (json.RawMessage, error) {
	respCh := make(chan json.RawMessage, 1)

	normalizedID := normalizeID(id)
	d.mu.Lock()
	d.pending[normalizedID] = respCh

	// Build and send the RPC request under the same lock (serializes writes)
	rpcReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      id,
	})
	err := d.ch.Send(ctx, rpcReq)
	d.mu.Unlock()

	if err != nil {
		d.mu.Lock()
		delete(d.pending, normalizedID)
		d.mu.Unlock()
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pending, normalizedID)
		d.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-respCh:
		if !ok {
			return nil, fmt.Errorf("channel closed while waiting for %s response", method)
		}
		return resp, nil
	case <-d.done:
		return nil, fmt.Errorf("demuxer stopped while waiting for %s response", method)
	}
}

// Stop closes the underlying channel to unblock the recv loop, then waits for exit.
// We close the channel rather than relying on context cancellation because
// NetControlChannel.Recv() only respects context deadlines, not cancellation.
func (d *channelDemuxer) Stop() {
	d.cancel()
	d.ch.Close()
	<-d.done
}

// Done returns a channel that is closed when the recv loop exits.
func (d *channelDemuxer) Done() <-chan struct{} {
	return d.done
}

// normalizeID converts JSON-decoded IDs (which are float64 for numbers) to a
// consistent type for map lookup.
func normalizeID(id interface{}) interface{} {
	switch v := id.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return v
	}
}
