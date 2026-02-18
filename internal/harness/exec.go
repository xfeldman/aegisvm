package harness

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"syscall"
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
