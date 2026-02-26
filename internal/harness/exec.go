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

// startPrimaryProcess starts the primary process and streams its output as log
// notifications. When the process exits, it sends a processExited notification
// with the exit code. If tracker.restartRequested is set, restarts the process
// automatically. Unlike exec, there is no exec_id — output is tagged as
// primary process output.
func startPrimaryProcess(ctx context.Context, params runParams, conn net.Conn, tracker *processTracker, hrpc *harnessRPC) (*exec.Cmd, error) {
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
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if err := sendNotification(conn, "log", logParams{Stream: "stdout", Line: scanner.Text()}); err != nil {
				return // conn dead, stop streaming to avoid blocking the process
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if err := sendNotification(conn, "log", logParams{Stream: "stderr", Line: scanner.Text()}); err != nil {
				return // conn dead, stop streaming to avoid blocking the process
			}
		}
	}()

	// Wait for process to finish and send processExited notification.
	// If restart was requested (self_restart), start a new process automatically.
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
		// Check restart flag BEFORE notifying daemon. If we send processExited
		// first, aegisd stops the VM and the restart can never happen.
		shouldRestart := false
		if tracker != nil {
			tracker.mu.Lock()
			shouldRestart = tracker.restartRequested
			tracker.restartRequested = false
			tracker.primary = nil
			tracker.mu.Unlock()
		}

		if shouldRestart {
			log.Println("restarting primary process (self_restart)")
			newCmd, err := startPrimaryProcess(ctx, params, conn, tracker, hrpc)
			if err != nil {
				log.Printf("restart primary: %v", err)
				// Restart failed — notify daemon of the exit
				sendNotification(conn, "processExited", map[string]interface{}{
					"exit_code": exitCode,
				})
				return
			}
			tracker.setPrimary(newCmd)
			if hrpc != nil {
				go monitorActivity(ctx, newCmd.Process.Pid, conn, hrpc)
			}
			return
		}

		sendNotification(conn, "processExited", map[string]interface{}{
			"exit_code": exitCode,
		})
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
			if err := sendNotification(conn, "log", execLogParams{
				Stream: "stdout",
				Line:   scanner.Text(),
				ExecID: params.ExecID,
			}); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if err := sendNotification(conn, "log", execLogParams{
				Stream: "stderr",
				Line:   scanner.Text(),
				ExecID: params.ExecID,
			}); err != nil {
				return
			}
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
