package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/vmm"
)

// Task states
const (
	TaskQueued    = "QUEUED"
	TaskRunning   = "RUNNING"
	TaskSucceeded = "SUCCEEDED"
	TaskFailed    = "FAILED"
	TaskTimedOut  = "TIMED_OUT"
)

// CreateTaskRequest is the request body for POST /v1/tasks.
type CreateTaskRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// Task represents a task in the system.
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

// LogLine is a single log line from task execution.
type LogLine struct {
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	Timestamp time.Time `json:"timestamp"`
}

// TaskStore manages tasks in memory (M0 — no SQLite).
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
	ll := LogLine{
		Stream:    stream,
		Line:      line,
		Timestamp: time.Now(),
	}
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
// 1. Start a TCP listener on the host (random port)
// 2. Boot a VM, passing the listener address as AEGIS_HOST_ADDR
// 3. The harness inside the VM connects back to us via TSI
// 4. Send runTask RPC over the connection
// 5. Stream logs, collect result
// 6. Shutdown and stop VM
func (ts *TaskStore) runTask(taskID string, req CreateTaskRequest) {
	ts.updateState(taskID, TaskRunning)

	// 1. Start a TCP listener on a random port for the harness to connect to
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("listen: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}
	defer ln.Close()

	hostAddr := ln.Addr().String()
	log.Printf("task %s: listening for harness connection on %s", taskID, hostAddr)

	// 2. Create and start VM
	vmCfg := vmm.VMConfig{
		RootfsPath: ts.cfg.BaseRootfsPath,
		MemoryMB:   ts.cfg.DefaultMemoryMB,
		VCPUs:      ts.cfg.DefaultVCPUs,
	}

	handle, err := ts.vmm.CreateVM(vmCfg)
	if err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("create VM: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}
	defer ts.vmm.StopVM(handle)

	// Set the host address for the harness to connect to
	libkrunVMM, ok := ts.vmm.(*vmm.LibkrunVMM)
	if !ok {
		ts.setResult(taskID, -1, "unsupported VMM backend")
		ts.updateState(taskID, TaskFailed)
		return
	}
	if err := libkrunVMM.SetHostAddr(handle, hostAddr); err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("set host addr: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}

	if err := ts.vmm.StartVM(handle); err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("start VM: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}

	// 3. Wait for the harness to connect (with timeout)
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("harness did not connect: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}
	defer conn.Close()
	log.Printf("task %s: harness connected from %s", taskID, conn.RemoteAddr())

	// 4. Send runTask RPC
	rpcReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "runTask",
		"params": map[string]interface{}{
			"command": req.Command,
			"env":     req.Env,
		},
		"id": 1,
	}

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(rpcReq); err != nil {
		ts.setResult(taskID, -1, fmt.Sprintf("send runTask: %v", err))
		ts.updateState(taskID, TaskFailed)
		return
	}

	// 5. Read responses (log notifications and final result)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var generic map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &generic); err != nil {
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

	if err := scanner.Err(); err != nil {
		log.Printf("task %s: read error: %v", taskID, err)
	}

	// 6. Send shutdown
	shutdownReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "shutdown",
		"params":  nil,
		"id":      2,
	}
	encoder.Encode(shutdownReq)
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
		for {
			select {
			case <-ctx.Done():
				return
			case ll, ok := <-ch:
				if !ok {
					return
				}
				streamJSON(w, ll)

				t, _ := s.tasks.getTask(id)
				if t != nil && t.State != TaskQueued && t.State != TaskRunning {
					return
				}
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

var _ = context.Background
