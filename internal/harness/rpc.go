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

type runTaskParams struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
}

type runTaskResult struct {
	ExitCode  int      `json:"exit_code"`
	Artifacts []string `json:"artifacts,omitempty"`
}

type healthResult struct {
	Status string `json:"status"`
}

type startServerParams struct {
	Command      []string          `json:"command"`
	Env          map[string]string `json:"env,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	ReadinessPort int              `json:"readiness_port"`
}

type startServerResult struct {
	PID int `json:"pid"`
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

// serverTracker tracks running server processes for cleanup on shutdown.
type serverTracker struct {
	mu      sync.Mutex
	servers []*exec.Cmd
}

func (st *serverTracker) add(cmd *exec.Cmd) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.servers = append(st.servers, cmd)
}

func (st *serverTracker) killAll() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, cmd := range st.servers {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

// handleConnection processes JSON-RPC messages from a single host connection.
func handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	tracker := &serverTracker{}
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
func dispatch(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *serverTracker) *rpcResponse {
	switch req.Method {
	case "runTask":
		return handleRunTask(ctx, req, conn)

	case "startServer":
		return handleStartServer(ctx, req, conn, tracker)

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

// handleRunTask executes a command and streams stdout/stderr as JSON-RPC notifications.
func handleRunTask(ctx context.Context, req *rpcRequest, conn net.Conn) *rpcResponse {
	var params runTaskParams
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

	log.Printf("runTask: %v", params.Command)

	exitCode, err := executeCommand(ctx, params, conn)
	if err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("execution error: %v", err),
			},
			ID: req.ID,
		}
	}

	return &rpcResponse{
		JSONRPC: "2.0",
		Result: runTaskResult{
			ExitCode: exitCode,
		},
		ID: req.ID,
	}
}

// handleStartServer starts a long-lived server process, polls its readiness port,
// and sends a serverReady notification when the port accepts connections.
func handleStartServer(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *serverTracker) *rpcResponse {
	var params startServerParams
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

	if params.ReadinessPort <= 0 {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "readiness_port is required",
			},
			ID: req.ID,
		}
	}

	log.Printf("startServer: %v (readiness port %d)", params.Command, params.ReadinessPort)

	cmd, err := startServerProcess(ctx, params, conn)
	if err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("start server: %v", err),
			},
			ID: req.ID,
		}
	}

	tracker.add(cmd)

	// Poll readiness in background, send notification when ready
	go func() {
		if err := waitForPort(params.ReadinessPort, 30*time.Second); err != nil {
			log.Printf("server readiness check failed: %v", err)
			sendNotification(conn, "serverFailed", map[string]string{
				"error": err.Error(),
			})
			return
		}
		log.Printf("server ready on port %d", params.ReadinessPort)
		sendNotification(conn, "serverReady", map[string]interface{}{
			"port": params.ReadinessPort,
		})
	}()

	return &rpcResponse{
		JSONRPC: "2.0",
		Result:  startServerResult{PID: cmd.Process.Pid},
		ID:      req.ID,
	}
}

// handleExec starts a command asynchronously in the guest, streaming output as log
// notifications with exec_id, and sending execDone when the process exits.
func handleExec(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *serverTracker) *rpcResponse {
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
