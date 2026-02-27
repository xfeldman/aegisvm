// Package daemon manages per-instance sidecar processes (e.g., gateways)
// declared by kit manifests. aegisd spawns one process per instance_daemon
// entry for each enabled instance that uses the kit.
//
// Lifecycle:
//   - Start on instance create/enable (if kit has instance_daemons)
//   - Keep running while instance is stopped/paused (wake-on-message)
//   - Stop on instance disable or delete
//   - Restart on crash with backoff
//   - Restore on daemon boot for enabled kit instances
package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/xfeldman/aegisvm/internal/kit"
)

// Manager manages per-instance sidecar daemon processes.
type Manager struct {
	mu         sync.Mutex
	procs      map[string][]*Process // instanceID → running daemon processes
	binDir     string
	dataDir    string
	socketPath string
}

// Process represents a running daemon process.
type Process struct {
	InstanceID string
	Handle     string
	Binary     string
	cmd        *exec.Cmd
	done       chan struct{} // closed when process exits
	stopOnce   sync.Once
}

// NewManager creates a daemon manager.
func NewManager(binDir, dataDir, socketPath string) *Manager {
	return &Manager{
		procs:      make(map[string][]*Process),
		binDir:     binDir,
		dataDir:    dataDir,
		socketPath: socketPath,
	}
}

// StartDaemons spawns instance daemons declared by the kit manifest.
// No-op if the instance has no kit or the kit has no instance_daemons.
// No-op if daemons are already running for this instance.
func (m *Manager) StartDaemons(instanceID, handle, kitName string) error {
	if kitName == "" {
		return nil
	}

	manifest, err := kit.LoadManifest(kitName)
	if err != nil {
		return fmt.Errorf("load kit manifest %q: %w", kitName, err)
	}
	if len(manifest.InstanceDaemons) == 0 {
		return nil
	}

	m.mu.Lock()
	if existing := m.procs[instanceID]; len(existing) > 0 {
		// Already running
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	var procs []*Process
	for _, d := range manifest.InstanceDaemons {
		p, err := m.spawn(instanceID, handle, d.Binary)
		if err != nil {
			// Stop any already-started daemons for this instance
			for _, started := range procs {
				started.stop()
			}
			return fmt.Errorf("spawn %s for %s: %w", d.Binary, handle, err)
		}
		procs = append(procs, p)
	}

	m.mu.Lock()
	m.procs[instanceID] = procs
	m.mu.Unlock()

	return nil
}

// StopDaemons stops all daemons for an instance.
func (m *Manager) StopDaemons(instanceID string) {
	m.mu.Lock()
	procs := m.procs[instanceID]
	delete(m.procs, instanceID)
	m.mu.Unlock()

	for _, p := range procs {
		p.stop()
	}
}

// StopAll stops all running daemons (daemon shutdown).
func (m *Manager) StopAll() {
	m.mu.Lock()
	all := make(map[string][]*Process)
	for k, v := range m.procs {
		all[k] = v
	}
	m.procs = make(map[string][]*Process)
	m.mu.Unlock()

	for _, procs := range all {
		for _, p := range procs {
			p.stop()
		}
	}
}

// IsRunning returns whether any daemon is running for the given instance.
func (m *Manager) IsRunning(instanceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	procs := m.procs[instanceID]
	for _, p := range procs {
		select {
		case <-p.done:
			continue // already exited
		default:
			return true
		}
	}
	return false
}

// spawn starts a single daemon binary for an instance.
func (m *Manager) spawn(instanceID, handle, binary string) (*Process, error) {
	binPath := filepath.Join(m.binDir, binary)
	if _, err := os.Stat(binPath); err != nil {
		return nil, fmt.Errorf("binary %q not found at %s", binary, binPath)
	}

	// Log output to ~/.aegis/data/{binary}-{handle}.log
	logName := fmt.Sprintf("%s-%s.log", binary, handle)
	logPath := filepath.Join(m.dataDir, logName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(binPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"AEGIS_INSTANCE="+handle,
		"AEGIS_SOCKET="+m.socketPath,
	)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	p := &Process{
		InstanceID: instanceID,
		Handle:     handle,
		Binary:     binary,
		cmd:        cmd,
		done:       make(chan struct{}),
	}

	log.Printf("daemon: started %s for instance %s (pid %d)", binary, handle, cmd.Process.Pid)

	// Monitor process in background, restart on crash with backoff
	go m.monitor(p, logFile)

	return p, nil
}

// monitor waits for process exit and restarts on crash with backoff.
func (m *Manager) monitor(p *Process, logFile *os.File) {
	defer logFile.Close()

	crashCount := 0
	lastStart := time.Now()

	for {
		err := p.cmd.Wait()

		// Check if we were intentionally stopped
		select {
		case <-p.done:
			return // intentional stop, don't restart
		default:
		}

		// Process exited — determine if it's a crash
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}

		// Clean exit (code 0) — don't restart (gateway found no config and exited intentionally)
		if exitCode == 0 {
			log.Printf("daemon: %s for %s exited cleanly", p.Binary, p.Handle)
			close(p.done)

			// Remove from tracked procs
			m.mu.Lock()
			m.removeProc(p.InstanceID, p)
			m.mu.Unlock()
			return
		}

		// Crash — backoff logic
		uptime := time.Since(lastStart)
		if uptime < 10*time.Second {
			crashCount++
		} else {
			crashCount = 1 // reset if ran for a while
		}

		if crashCount >= 5 {
			log.Printf("daemon: %s for %s crash loop (%d crashes), giving up", p.Binary, p.Handle, crashCount)
			close(p.done)
			m.mu.Lock()
			m.removeProc(p.InstanceID, p)
			m.mu.Unlock()
			return
		}

		delay := time.Duration(crashCount) * time.Second
		log.Printf("daemon: %s for %s crashed (exit %d), restarting in %v", p.Binary, p.Handle, exitCode, delay)
		time.Sleep(delay)

		// Check if stopped during sleep
		select {
		case <-p.done:
			return
		default:
		}

		// Restart
		binPath := filepath.Join(m.binDir, p.Binary)
		newCmd := exec.Command(binPath)
		newCmd.Stdout = logFile
		newCmd.Stderr = logFile
		newCmd.Env = append(os.Environ(),
			"AEGIS_INSTANCE="+p.Handle,
			"AEGIS_SOCKET="+m.socketPath,
		)

		if err := newCmd.Start(); err != nil {
			log.Printf("daemon: restart %s for %s failed: %v", p.Binary, p.Handle, err)
			close(p.done)
			m.mu.Lock()
			m.removeProc(p.InstanceID, p)
			m.mu.Unlock()
			return
		}

		p.cmd = newCmd
		lastStart = time.Now()
		log.Printf("daemon: restarted %s for %s (pid %d)", p.Binary, p.Handle, newCmd.Process.Pid)
	}
}

// removeProc removes a process from the tracked map.
func (m *Manager) removeProc(instanceID string, p *Process) {
	procs := m.procs[instanceID]
	for i, pp := range procs {
		if pp == p {
			m.procs[instanceID] = append(procs[:i], procs[i+1:]...)
			break
		}
	}
	if len(m.procs[instanceID]) == 0 {
		delete(m.procs, instanceID)
	}
}

// stop sends SIGTERM and waits up to 5 seconds for the process to exit.
func (p *Process) stop() {
	p.stopOnce.Do(func() {
		close(p.done) // signal monitor to stop

		if p.cmd.Process == nil {
			return
		}

		p.cmd.Process.Signal(os.Interrupt)

		// Wait up to 5 seconds
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()

		exited := make(chan struct{})
		go func() {
			p.cmd.Wait()
			close(exited)
		}()

		select {
		case <-exited:
			log.Printf("daemon: %s for %s stopped", p.Binary, p.Handle)
		case <-timer.C:
			p.cmd.Process.Kill()
			log.Printf("daemon: %s for %s killed (timeout)", p.Binary, p.Handle)
		}
	})
}
