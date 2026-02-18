//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var binDir string

func TestMain(m *testing.M) {
	// Find binaries relative to the repo root
	root := repoRoot()
	binDir = filepath.Join(root, "bin")

	if _, err := os.Stat(filepath.Join(binDir, "aegis")); err != nil {
		fmt.Fprintf(os.Stderr, "binaries not found at %s — run 'make all' first\n", binDir)
		os.Exit(1)
	}

	// Ensure daemon is stopped before we start
	aegis("down")
	time.Sleep(500 * time.Millisecond)

	// Start daemon
	cmd := exec.Command(filepath.Join(binDir, "aegisd"))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start aegisd: %v\n", err)
		os.Exit(1)
	}

	// Wait for daemon to be ready
	ready := false
	for i := 0; i < 30; i++ {
		if daemonRunning() {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintln(os.Stderr, "aegisd did not start within timeout")
		cmd.Process.Kill()
		os.Exit(1)
	}

	code := m.Run()

	// Tear down
	aegis("down")
	time.Sleep(500 * time.Millisecond)

	os.Exit(code)
}

func repoRoot() string {
	// Walk up from the test file to find go.mod
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// fallback
			return "."
		}
		dir = parent
	}
}

func aegis(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "aegis"), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String() + stderr.String()
	return strings.TrimSpace(out), err
}

func aegisRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := aegis(args...)
	if err != nil {
		t.Fatalf("aegis %v failed: %v\noutput: %s", args, err, out)
	}
	return out
}

func daemonRunning() bool {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".aegis", "data", "aegisd.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func daemonClient() *http.Client {
	home, _ := os.UserHomeDir()
	sockPath := filepath.Join(home, ".aegis", "aegisd.sock")
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 5*time.Second)
			},
		},
		Timeout: 2 * time.Minute,
	}
}

func apiPost(t *testing.T, path string, body interface{}) map[string]interface{} {
	t.Helper()
	bodyJSON, _ := json.Marshal(body)
	client := daemonClient()
	resp, err := client.Post("http://aegis"+path, "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	if resp.StatusCode >= 400 {
		t.Fatalf("POST %s returned %d: %s", path, resp.StatusCode, data)
	}
	return result
}

func apiDelete(t *testing.T, path string) {
	t.Helper()
	client := daemonClient()
	req, _ := http.NewRequest("DELETE", "http://aegis"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	resp.Body.Close()
}

func apiPut(t *testing.T, path string, body interface{}) map[string]interface{} {
	t.Helper()
	bodyJSON, _ := json.Marshal(body)
	client := daemonClient()
	req, _ := http.NewRequest("PUT", "http://aegis"+path, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	if resp.StatusCode >= 400 {
		t.Fatalf("PUT %s returned %d: %s", path, resp.StatusCode, data)
	}
	return result
}

func apiDeleteAllowFail(t *testing.T, path string) {
	t.Helper()
	client := daemonClient()
	req, _ := http.NewRequest("DELETE", "http://aegis"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func waitForTaskOutput(t *testing.T, client *http.Client, taskID string, timeout time.Duration) string {
	t.Helper()

	// Follow logs
	logsResp, err := client.Get(fmt.Sprintf("http://aegis/v1/tasks/%s/logs?follow=true", taskID))
	if err != nil {
		t.Fatalf("follow logs: %v", err)
	}
	defer logsResp.Body.Close()

	var lines []string
	decoder := json.NewDecoder(logsResp.Body)
	for decoder.More() {
		var logLine map[string]interface{}
		if err := decoder.Decode(&logLine); err != nil {
			break
		}
		if line, ok := logLine["line"].(string); ok {
			lines = append(lines, line)
		}
	}

	// Wait briefly and check final status
	time.Sleep(200 * time.Millisecond)
	return strings.Join(lines, "\n")
}

func waitForHTTP(url string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 200 {
				return string(body), nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("timeout waiting for %s", url)
}
