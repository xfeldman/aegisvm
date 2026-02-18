package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/vmm"
)

const (
	TaskQueued    = "QUEUED"
	TaskRunning   = "RUNNING"
	TaskSucceeded = "SUCCEEDED"
	TaskFailed    = "FAILED"
	TaskTimedOut  = "TIMED_OUT"
)

type CreateTaskRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

type Task struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Command   []string   `json:"command"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type LogLine struct {
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	Timestamp time.Time `json:"timestamp"`
}

type TaskStore struct {
	mu    sync.Mutex
	tasks map[string]*taskEntry
	vmm   vmm.VMM
	cfg   *config.Config
	seq   int
}

type taskEntry struct {
	task Task
	logs []LogLine
	subs []chan LogLine
}

func NewTaskStore(v vmm.VMM, cfg *config.Config) *TaskStore {
	return &TaskStore{
		tasks: make(map[string]*taskEntry),
		vmm:   v,
		cfg:   cfg,
	}
}

func (ts *TaskStore) createTask(cmd []string) *Task {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.seq++
	id := fmt.Sprintf("task-%d", ts.seq)
	t := &Task{
		ID:        id,
		State:     TaskQueued,
		Command:   cmd,
		CreatedAt: time.Now(),
	}
	ts.tasks[id] = &taskEntry{task: *t}
	return t
}

func (ts *TaskStore) getTask(id string) (*Task, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.tasks[id]
	if !ok {
		return nil, false
	}
	t := entry.task
	return &t, true
}

func (ts *TaskStore) updateState(id, state string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.tasks[id]
	if !ok {
		return
	}
	entry.task.State = state
	now := time.Now()
	switch state {
	case TaskRunning:
		entry.task.StartedAt = &now
	case TaskSucceeded, TaskFailed, TaskTimedOut:
		entry.task.EndedAt = &now
	}
}

func (ts *TaskStore) setResult(id string, exitCode int, errMsg string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.tasks[id]
	if !ok {
		return
	}
	entry.task.ExitCode = &exitCode
	entry.task.Error = errMsg
}

func (ts *TaskStore) appendLog(id string, stream, line string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.tasks[id]
	if !ok {
		return
	}
	ll := LogLine{Stream: stream, Line: line, Timestamp: time.Now()}
	entry.logs = append(entry.logs, ll)
	for _, ch := range entry.subs {
		select {
		case ch <- ll:
		default:
		}
	}
}

func (ts *TaskStore) subscribeLogs(id string) (chan LogLine, []LogLine, func()) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.tasks[id]
	if !ok {
		return nil, nil, func() {}
	}
	ch := make(chan LogLine, 100)
	entry.subs = append(entry.subs, ch)
	existing := make([]LogLine, len(entry.logs))
	copy(existing, entry.logs)
	unsub := func() {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		for i, s := range entry.subs {
			if s == ch {
				entry.subs = append(entry.subs[:i], entry.subs[i+1:]...)
				break
			}
		}
		close(ch)
	}
	return ch, existing, unsub
}

// runTask executes a task end-to-end:
// 1. Create VM with RootFS config
// 2. StartVM — backend handles transport setup, returns ControlChannel
// 3. Send runTask RPC over the channel
// 4. Stream logs, collect result
// 5. Shutdown and stop VM
//
// Core never touches TCP, vsock, or unix sockets — only ControlChannel.Send/Recv.
func (ts *TaskStore) runTask(taskID string, req CreateTaskRequest) {
	ts.updateState(taskID, TaskRunning)

	vmCfg := vmm.VMConfig{
		Rootfs: vmm.RootFS{
			Type: ts.vmm.Capabilities().RootFSType,
			Path: ts.cfg.BaseRootfsPath,
		},
		MemoryMB: ts.cfg.DefaultMemoryMB,
		VCPUs:    ts.cfg.DefaultVCPUs,
	}

	handle, err := ts.vmm.CreateVM(vmCfg)
	if err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("create VM: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}
	defer ts.vmm.StopVM(handle)

	ch, err := ts.vmm.StartVM(handle)
	if err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("start VM: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}
	defer ch.Close()
	log.Printf("task %s: harness connected", taskID)

	// Task-scoped context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Send runTask RPC
	rpcReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "runTask",
		"params": map[string]interface{}{
			"command": req.Command,
			"env":     req.Env,
		},
		"id": 1,
	})
	if err := ch.Send(ctx, rpcReq); err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("send runTask: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}

	// Read responses (log notifications and final result)
	for {
		msg, err := ch.Recv(ctx)
		if err != nil {
			log.Printf("task %s: recv error: %v", taskID, err)
			if ctx.Err() != nil {
				ts.setResult(taskID, -1, "task timed out")
				ts.updateState(taskID, TaskTimedOut)
			}
			break
		}

		var generic map[string]interface{}
		if err := json.Unmarshal(msg, &generic); err != nil {
			continue
		}

		if method, _ := generic["method"].(string); method == "log" {
			if params, ok := generic["params"].(map[string]interface{}); ok {
				stream, _ := params["stream"].(string)
				line, _ := params["line"].(string)
				ts.appendLog(taskID, stream, line)
			}
			continue
		}

		if _, hasID := generic["id"]; hasID {
			if errObj, ok := generic["error"].(map[string]interface{}); ok {
				errMsg, _ := errObj["message"].(string)
				ts.setResult(taskID, -1, errMsg)
				ts.updateState(taskID, TaskFailed)
			} else if result, ok := generic["result"].(map[string]interface{}); ok {
				exitCode := 0
				if ec, ok := result["exit_code"].(float64); ok {
					exitCode = int(ec)
				}
				ts.setResult(taskID, exitCode, "")
				if exitCode == 0 {
					ts.updateState(taskID, TaskSucceeded)
				} else {
					ts.updateState(taskID, TaskFailed)
				}
			}
			break
		}
	}

	// Send shutdown (short deadline — don't block if guest is stuck)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	shutdownReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "shutdown",
		"params":  nil,
		"id":      2,
	})
	ch.Send(shutdownCtx, shutdownReq)
}

// HTTP handlers

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	task := s.tasks.createTask(req.Command)
	log.Printf("task %s created: %v", task.ID, req.Command)
	go s.tasks.runTask(task.ID, req)
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	task, ok := s.tasks.getTask(id)
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleGetTaskLogs(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	follow := r.URL.Query().Get("follow") == "true"

	task, ok := s.tasks.getTask(id)
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if follow && (task.State == TaskQueued || task.State == TaskRunning) {
		ch, existing, unsub := s.tasks.subscribeLogs(id)
		if ch == nil {
			return
		}
		defer unsub()
		for _, ll := range existing {
			streamJSON(w, ll)
		}
		ctx := r.Context()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ll, ok := <-ch:
				if !ok {
					return
				}
				streamJSON(w, ll)
			case <-ticker.C:
				// Periodically check if task is done (in case last log
				// arrived before state transitioned to terminal)
			}
			t, _ := s.tasks.getTask(id)
			if t != nil && t.State != TaskQueued && t.State != TaskRunning {
				return
			}
		}
	} else {
		_, existing, unsub := s.tasks.subscribeLogs(id)
		unsub()
		for _, ll := range existing {
			streamJSON(w, ll)
		}
	}
}
