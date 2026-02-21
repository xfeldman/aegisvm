package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gvproxyInstance manages a gvproxy child process that provides virtio-net
// networking for a single VM. Each VM gets its own gvproxy.
type gvproxyInstance struct {
	cmd       *exec.Cmd
	netSocket string // unixgram socket (data plane — virtio-net)
	apiSocket string // unix stream socket (control — port forwarding)
	pidFile   string
}

// startGvproxy spawns a gvproxy process for the given VM.
// sockDir is the directory for unix sockets (e.g. ~/.aegis/data/sockets).
// gvproxyBin is the absolute path to the gvproxy binary.
func startGvproxy(gvproxyBin, vmID, sockDir string) (*gvproxyInstance, error) {
	netSock := filepath.Join(sockDir, fmt.Sprintf("net-%s.sock", vmID))
	apiSock := filepath.Join(sockDir, fmt.Sprintf("api-%s.sock", vmID))
	pidFile := filepath.Join(sockDir, fmt.Sprintf("gvproxy-%s.pid", vmID))

	// Clean up stale sockets from previous runs
	os.Remove(netSock)
	os.Remove(apiSock)

	cmd := exec.Command(gvproxyBin,
		"--listen-vfkit", "unixgram://"+netSock,
		"--listen", "unix://"+apiSock,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gvproxy: %w", err)
	}

	// Write PID file for orphan reaping
	os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600)

	g := &gvproxyInstance{
		cmd:       cmd,
		netSocket: netSock,
		apiSocket: apiSock,
		pidFile:   pidFile,
	}

	// Wait for API socket to become available
	if err := g.waitForAPI(5 * time.Second); err != nil {
		g.Stop()
		return nil, fmt.Errorf("gvproxy API not ready: %w", err)
	}

	return g, nil
}

// waitForAPI polls the API socket until it accepts connections or times out.
func (g *gvproxyInstance) waitForAPI(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", g.apiSocket, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", g.apiSocket)
}

// ExposePort creates a port forwarding rule: hostPort on localhost → guestIP:guestPort.
func (g *gvproxyInstance) ExposePort(hostPort, guestPort int) error {
	body := map[string]string{
		"local":  fmt.Sprintf("127.0.0.1:%d", hostPort),
		"remote": fmt.Sprintf("192.168.127.2:%d", guestPort),
	}
	return g.apiPost("/services/forwarder/expose", body)
}

// UnexposePort removes a port forwarding rule.
func (g *gvproxyInstance) UnexposePort(hostPort int) error {
	body := map[string]string{
		"local": fmt.Sprintf("127.0.0.1:%d", hostPort),
	}
	return g.apiPost("/services/forwarder/unexpose", body)
}

// Stop kills the gvproxy process and cleans up sockets/PID files.
func (g *gvproxyInstance) Stop() {
	if g.cmd != nil && g.cmd.Process != nil {
		g.cmd.Process.Kill()
		g.cmd.Wait()
	}
	os.Remove(g.netSocket)
	os.Remove(g.apiSocket)
	os.Remove(g.pidFile)
}

// apiPost sends a POST request to the gvproxy HTTP API via unix socket.
func (g *gvproxyInstance) apiPost(path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", g.apiSocket, 5*time.Second)
			},
		},
		Timeout: 10 * time.Second,
	}

	resp, err := client.Post("http://gvproxy"+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gvproxy API %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gvproxy API %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// ReapOrphanGvproxies kills leftover gvproxy processes from previous daemon runs.
// Call this at aegisd startup before restoring instances.
func ReapOrphanGvproxies(sockDir string) {
	pattern := filepath.Join(sockDir, "gvproxy-*.pid")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return
	}

	for _, pidFile := range matches {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			os.Remove(pidFile)
			continue
		}

		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			os.Remove(pidFile)
			continue
		}

		proc, err := os.FindProcess(pid)
		if err == nil {
			// Signal 0 checks if process exists without killing it
			if proc.Signal(nil) == nil {
				log.Printf("reaping orphan gvproxy (pid %d)", pid)
				proc.Kill()
				proc.Wait()
			}
		}

		// Clean up associated sockets
		base := strings.TrimSuffix(filepath.Base(pidFile), ".pid")
		vmID := strings.TrimPrefix(base, "gvproxy-")
		os.Remove(filepath.Join(sockDir, fmt.Sprintf("net-%s.sock", vmID)))
		os.Remove(filepath.Join(sockDir, fmt.Sprintf("api-%s.sock", vmID)))
		os.Remove(pidFile)
	}
}
