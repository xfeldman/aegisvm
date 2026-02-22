// Package tether defines the bidirectional framed message protocol between
// host and guest for the Aegis Agent Kit.
//
// Tether rides on the existing control channel (JSON-RPC 2.0 over vsock).
// All frames are sent as "tether.frame" notifications. Direction is inferred
// from the frame type prefix:
//
//   - user.*, control.*  → ingress (host → guest)
//   - assistant.*, status.*, event.*, error  → egress (guest → host)
package tether

import "encoding/json"

// Frame is the tether envelope. Every tether message — user input, streaming
// assistant output, presence signals, control — uses this single structure.
type Frame struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	TS      string          `json:"ts,omitempty"`
	Session SessionID       `json:"session"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SessionID identifies a conversation session across channels.
type SessionID struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

// IsIngress returns true if this frame flows host → guest.
func (f *Frame) IsIngress() bool {
	switch {
	case len(f.Type) >= 5 && f.Type[:5] == "user.":
		return true
	case len(f.Type) >= 8 && f.Type[:8] == "control.":
		return true
	default:
		return false
	}
}

// IsEgress returns true if this frame flows guest → host.
func (f *Frame) IsEgress() bool {
	return !f.IsIngress()
}
