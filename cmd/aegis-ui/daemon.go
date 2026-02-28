//go:build uifrontend

package main

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/version"
)

func aegisDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis")
}

func socketPath() string {
	return filepath.Join(aegisDir(), "aegisd.sock")
}

func pidFilePath() string {
	return filepath.Join(aegisDir(), "data", "aegisd.pid")
}

func isDaemonRunning() bool {
	data, err := os.ReadFile(pidFilePath())
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
	err = proc.Signal(syscall.Signal(0))
	// err == nil: process alive, we can signal it
	// EPERM: process alive but owned by root (daemon runs via sudo on Linux)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ensureDesktopSetup creates symlinks and extracts config from the .app bundle.
//
// Unlike the old ensureBinaries() which copied all executables + dylibs to
// ~/.aegis/bin/ and ~/.aegis/lib/, this follows the macOS convention of running
// binaries directly from the .app bundle (like Docker Desktop). Only two
// symlinks are created for CLI/MCP access, plus the kit manifest is extracted
// as user config. Dragging the .app to Trash cleanly removes all executables.
//
// No-op when not running from a bundle (dev mode).
func ensureDesktopSetup() {
	if runtime.GOOS != "darwin" {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	// Check if running inside an .app bundle: MacOS/../Resources/
	resourcesDir := filepath.Join(filepath.Dir(exe), "..", "Resources")
	if _, err := os.Stat(resourcesDir); err != nil {
		return // Not in a bundle (dev mode)
	}

	binDir := filepath.Join(aegisDir(), "bin")
	os.MkdirAll(binDir, 0755)

	// Version check: skip setup if already at current version
	versionFile := filepath.Join(binDir, ".version")
	currentVersion := version.Version()
	if data, err := os.ReadFile(versionFile); err == nil {
		if strings.TrimSpace(string(data)) == currentVersion {
			return
		}
	}

	// Create symlinks for CLI and MCP server.
	// These are the only binaries users invoke directly; everything else
	// (aegisd, vmm-worker, harness, gateway, agent) runs from the .app bundle.
	for _, name := range []string{"aegis", "aegis-mcp"} {
		src := filepath.Join(resourcesDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(binDir, name)
		os.Remove(dst) // Remove old copy or stale symlink
		if err := os.Symlink(src, dst); err != nil {
			log.Printf("aegis-ui: symlink %s: %v", name, err)
		}
	}

	// Clean up old binary copies from previous versions that used ensureBinaries().
	// These are no longer needed — aegisd runs from the .app bundle now.
	for _, name := range []string{"aegisd", "aegis-harness", "aegis-vmm-worker", "aegis-gateway", "aegis-agent", "aegis-mcp-guest"} {
		old := filepath.Join(binDir, name)
		if info, err := os.Lstat(old); err == nil && info.Mode().IsRegular() {
			os.Remove(old)
		}
	}

	// Clean up old dylib copies from previous versions.
	oldLibDir := filepath.Join(aegisDir(), "lib")
	if entries, err := os.ReadDir(oldLibDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".dylib") {
				os.Remove(filepath.Join(oldLibDir, e.Name()))
			}
		}
		// Remove the directory if empty
		os.Remove(oldLibDir)
	}

	// Extract kit manifest (config, not a binary — belongs in user dir)
	kitSrc := filepath.Join(resourcesDir, "kits", "agent.json")
	if _, err := os.Stat(kitSrc); err == nil {
		kitDir := filepath.Join(aegisDir(), "kits")
		os.MkdirAll(kitDir, 0755)
		copyFile(kitSrc, filepath.Join(kitDir, "agent.json"))
	}

	// Write version marker
	os.WriteFile(versionFile, []byte(currentVersion), 0644)

	log.Printf("aegis-ui: desktop setup complete (version %s)", currentVersion)
}

// findAegisdBinary locates the aegisd binary.
// Search order:
//  1. .app/Contents/Resources/aegisd (primary for desktop app — run from bundle)
//  2. ~/.aegis/bin/aegisd (backwards compat with old binary copies)
//  3. Next to this executable (dev mode — all binaries in bin/)
func findAegisdBinary() string {
	exe, _ := os.Executable()

	// 1. Inside .app bundle (primary for desktop app)
	if exe != "" {
		candidate := filepath.Join(filepath.Dir(exe), "..", "Resources", "aegisd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 2. Extracted/installed binaries (backwards compat, Homebrew symlinks)
	candidate := filepath.Join(aegisDir(), "bin", "aegisd")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// 3. Next to this executable (dev mode)
	if exe != "" {
		candidate = filepath.Join(filepath.Dir(exe), "aegisd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return ""
}

// ensureDaemon starts aegisd if it's not already running.
// Non-fatal: if the daemon can't be started, the UI will show
// the "aegisd not running" error state and the user can start it manually.
func ensureDaemon() {
	if isDaemonRunning() {
		return
	}

	aegisdBin := findAegisdBinary()
	if aegisdBin == "" {
		log.Println("aegis-ui: aegisd binary not found — UI will show disconnected state")
		return
	}

	startDaemon(aegisdBin)
}

func startDaemon(aegisdBin string) {
	// Create log directory and file
	logDir := filepath.Join(aegisDir(), "data")
	os.MkdirAll(logDir, 0755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "aegisd.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("aegis-ui: create log file: %v", err)
		return
	}

	// Some backends (Cloud Hypervisor on Linux) need root
	platform, _ := config.DetectPlatform()
	needsSudo := platform != nil && platform.NeedsRoot && os.Geteuid() != 0

	var cmd *exec.Cmd
	if needsSudo {
		cmd = exec.Command("sudo", "--preserve-env=HOME", aegisdBin)
	} else {
		cmd = exec.Command(aegisdBin)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Own process group so the daemon isn't killed when the desktop app exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Printf("aegis-ui: start aegisd: %v", err)
		return
	}

	// Monitor for early exit
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// Wait for daemon to be ready
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 10; i++ {
		if isDaemonRunning() {
			log.Println("aegis-ui: aegisd started")
			return
		}
		select {
		case <-exited:
			log.Println("aegis-ui: aegisd exited immediately — check ~/.aegis/data/aegisd.log")
			return
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Println("aegis-ui: aegisd did not start within timeout")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
