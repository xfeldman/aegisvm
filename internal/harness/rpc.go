package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"
)

// JSON-RPC 2.0 message types

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

type rpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// RPC params/results

type runParams struct {
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	ExposePorts []int             `json:"expose_ports,omitempty"` // guest ports to proxy (0.0.0.0 → 127.0.0.1)
}

type runResult struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

type healthResult struct {
	Status string `json:"status"`
}

type logParams struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Line   string `json:"line"`
}

type execParams struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	ExecID  string            `json:"exec_id"`
}

type execLogParams struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
	ExecID string `json:"exec_id,omitempty"`
}

type execResult struct {
	ExecID    string `json:"exec_id"`
	StartedAt string `json:"started_at"`
}

// processTracker tracks running processes for cleanup on shutdown.
type processTracker struct {
	mu        sync.Mutex
	processes []*exec.Cmd
	primary   *exec.Cmd // primary process started by `run` RPC
}

func (pt *processTracker) add(cmd *exec.Cmd) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.processes = append(pt.processes, cmd)
}

func (pt *processTracker) setPrimary(cmd *exec.Cmd) bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.primary != nil {
		return false // already running
	}
	pt.primary = cmd
	pt.processes = append(pt.processes, cmd)
	return true
}

func (pt *processTracker) killAll() {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	for _, cmd := range pt.processes {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

// handleConnection processes JSON-RPC messages from a single host connection.
func handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	tracker := &processTracker{}
	defer tracker.killAll()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message
	encoder := json.NewEncoder(conn)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid JSON-RPC message: %v", err)
			continue
		}

		if req.JSONRPC != "2.0" {
			log.Printf("invalid JSON-RPC version: %s", req.JSONRPC)
			continue
		}

		resp := dispatch(ctx, &req, conn, tracker)
		if resp != nil {
			if err := encoder.Encode(resp); err != nil {
				log.Printf("write response: %v", err)
				return
			}
		}

		// If shutdown was requested, exit
		if req.Method == "shutdown" {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("read error: %v", err)
	}
}

// dispatch routes a JSON-RPC request to the appropriate handler.
func dispatch(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker) *rpcResponse {
	switch req.Method {
	case "run":
		return handleRun(ctx, req, conn, tracker)

	case "exec":
		return handleExec(ctx, req, conn, tracker)

	case "health":
		return &rpcResponse{
			JSONRPC: "2.0",
			Result:  healthResult{Status: "ok"},
			ID:      req.ID,
		}

	case "shutdown":
		log.Println("shutdown requested")
		return &rpcResponse{
			JSONRPC: "2.0",
			Result:  map[string]string{"status": "shutting_down"},
			ID:      req.ID,
		}

	default:
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
			ID: req.ID,
		}
	}
}

// handleRun starts the primary process asynchronously, streaming output as log
// notifications. When the process exits, it sends a processExited notification.
// Only one primary process is allowed per instance.
func handleRun(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker) *rpcResponse {
	var params runParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: fmt.Sprintf("invalid params: %v", err),
			},
			ID: req.ID,
		}
	}

	if len(params.Command) == 0 {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "command is required",
			},
			ID: req.ID,
		}
	}

	log.Printf("run: %v", params.Command)

	// Start port proxies for exposed ports before the app.
	// These forward 0.0.0.0:port → 127.0.0.1:port so that apps binding
	// to localhost are reachable via gvproxy's virtio-net ingress.
	var pp *portProxy
	if len(params.ExposePorts) > 0 {
		pp = startPortProxies(params.ExposePorts)
	}

	cmd, err := startPrimaryProcess(ctx, params, conn)
	if err != nil {
		if pp != nil {
			pp.Stop()
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("start process: %v", err),
			},
			ID: req.ID,
		}
	}

	if !tracker.setPrimary(cmd) {
		cmd.Process.Kill()
		if pp != nil {
			pp.Stop()
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: "primary process already running",
			},
			ID: req.ID,
		}
	}

	return &rpcResponse{
		JSONRPC: "2.0",
		Result: runResult{
			PID:       cmd.Process.Pid,
			StartedAt: time.Now().Format(time.RFC3339),
		},
		ID: req.ID,
	}
}

// handleExec starts a command asynchronously in the guest, streaming output as log
// notifications with exec_id, and sending execDone when the process exits.
func handleExec(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker) *rpcResponse {
	var params execParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: fmt.Sprintf("invalid params: %v", err),
			},
			ID: req.ID,
		}
	}

	if len(params.Command) == 0 {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "command is required",
			},
			ID: req.ID,
		}
	}

	if params.ExecID == "" {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "exec_id is required",
			},
			ID: req.ID,
		}
	}

	log.Printf("exec: %v (exec_id=%s)", params.Command, params.ExecID)

	cmd, err := startExecProcess(ctx, params, conn)
	if err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("exec: %v", err),
			},
			ID: req.ID,
		}
	}

	tracker.add(cmd)

	return &rpcResponse{
		JSONRPC: "2.0",
		Result: execResult{
			ExecID:    params.ExecID,
			StartedAt: time.Now().Format(time.RFC3339),
		},
		ID: req.ID,
	}
}

// sendNotification sends a JSON-RPC notification (no ID, no response expected).
func sendNotification(conn net.Conn, method string, params interface{}) error {
	notif := rpcNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}
