package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
)

// TetherFrame is the tether envelope sent/received via the harness.
type TetherFrame struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	TS      string          `json:"ts,omitempty"`
	Session SessionID       `json:"session"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SessionID identifies a conversation session.
type SessionID struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

// handleTetherRecv receives a tether frame from the harness.
func (a *Agent) handleTetherRecv(w http.ResponseWriter, r *http.Request) {
	var frame TetherFrame
	if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch frame.Type {
	case "user.message":
		go a.handleUserMessage(frame)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) sendPresence(session SessionID, state string) {
	a.sendFrame(TetherFrame{
		V: 1, Type: "status.presence", TS: now(), Session: session,
		Payload: mustMarshal(map[string]string{"state": state}),
	})
}

func (a *Agent) sendDelta(session SessionID, text string) {
	a.sendFrame(TetherFrame{
		V: 1, Type: "assistant.delta", TS: now(), Session: session,
		Payload: mustMarshal(map[string]string{"text": text}),
	})
}

func (a *Agent) sendDone(session SessionID, text string) {
	a.sendFrame(TetherFrame{
		V: 1, Type: "assistant.done", TS: now(), Session: session,
		Payload: mustMarshal(map[string]string{"text": text}),
	})
}

func (a *Agent) sendFrame(frame TetherFrame) {
	data, _ := json.Marshal(frame)
	resp, err := http.Post(harnessAPI+"/v1/tether/send", "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("send tether frame: %v", err)
		return
	}
	resp.Body.Close()
}
