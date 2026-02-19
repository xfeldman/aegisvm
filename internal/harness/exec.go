package harness

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"syscall"
	"time"
)

// executeCommand runs a command and streams stdout/stderr as JSON-RPC log notifications.
// Returns the exit code and any error.
func executeCommand(ctx context.Context, params runTaskParams, conn net.Conn) (int, error) {
	if len(params.Command) == 0 {
		return -1, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, params.Command[0], params.Command[1:]...)

	if params.Workdir != "" {
		cmd.Dir = params.Workdir
	}

	// Build environment
	if len(params.Env) > 0 {
		env := cmd.Environ()
		for k, v := range params.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	// Capture stdout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture stderr
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start command: %w", err)
	}

	// Stream stdout and stderr as JSON-RPC notifications
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if err := sendNotification(conn, "log", logParams{Stream: "stdout", Line: line}); err != nil {
				log.Printf("send stdout notification: %v", err)
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if err := sendNotification(conn, "log", logParams{Stream: "stderr", Line: line}); err != nil {
				log.Printf("send stderr notification: %v", err)
				return
			}
		}
	}()

	// Wait for both streams to finish
	<-done
	<-done

	// Wait for process to exit
	err = cmd.Wait()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			} else {
				exitCode = 1
			}
		} else {
			return -1, fmt.Errorf("wait: %w", err)
		}
	}

	return exitCode, nil
}

// startServerProcess starts a long-lived server process and streams its output.
// Unlike executeCommand, it returns immediately after starting the process.
// The caller is responsible for killing the process.
func startServerProcess(ctx context.Context, params startServerParams, conn net.Conn) (*exec.Cmd, error) {
	if len(params.Command) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, params.Command[0], params.Command[1:]...)

	if params.Workdir != "" {
		cmd.Dir = params.Workdir
	}

	if len(params.Env) > 0 {
		env := cmd.Environ()
		for k, v := range params.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	// Stream stdout/stderr as log notifications in background
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			sendNotification(conn, "log", logParams{Stream: "stdout", Line: scanner.Text()})
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			sendNotification(conn, "log", logParams{Stream: "stderr", Line: scanner.Text()})
		}
	}()

	return cmd, nil
}

// startExecProcess starts an exec command asynchronously, streaming output as log
// notifications with exec_id. When the process exits, it sends an execDone notification.
func startExecProcess(ctx context.Context, params execParams, conn net.Conn) (*exec.Cmd, error) {
	if len(params.Command) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, params.Command[0], params.Command[1:]...)

	if params.Workdir != "" {
		cmd.Dir = params.Workdir
	}

	if len(params.Env) > 0 {
		env := cmd.Environ()
		for k, v := range params.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	// Stream stdout/stderr as log notifications with exec_id
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			sendNotification(conn, "log", execLogParams{
				Stream: "stdout",
				Line:   scanner.Text(),
				ExecID: params.ExecID,
			})
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			sendNotification(conn, "log", execLogParams{
				Stream: "stderr",
				Line:   scanner.Text(),
				ExecID: params.ExecID,
			})
		}
	}()

	// Wait for process to finish and send execDone notification
	go func() {
		<-done
		<-done
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					exitCode = status.ExitStatus()
				} else {
					exitCode = 1
				}
			} else {
				exitCode = -1
			}
		}
		sendNotification(conn, "execDone", map[string]interface{}{
			"exec_id":   params.ExecID,
			"exit_code": exitCode,
		})
	}()

	return cmd, nil
}

// waitForPort polls a TCP port until it accepts connections or the timeout expires.
func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d not ready after %v", port, timeout)
}
