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

// bundledBinaries are the binary names extracted from the .app bundle
// to ~/.aegis/bin/ on first launch or version mismatch.
var bundledBinaries = []string{
	"aegis",
	"aegisd",
	"aegis-harness",
	"aegis-vmm-worker",
	"aegis-gateway",
	"aegis-agent",
	"aegis-mcp",
	"aegis-mcp-guest",
}

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

// ensureBinaries extracts bundled binaries from the .app bundle (macOS)
// to ~/.aegis/bin/ on first launch or when the version changes.
// No-op when not running from a bundle (dev mode).
func ensureBinaries() {
	if runtime.GOOS != "darwin" {
		return // Only macOS .app bundles for now
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

	// Target directory
	binDir := filepath.Join(aegisDir(), "bin")
	os.MkdirAll(binDir, 0755)

	// Version check: skip extraction if already at current version
	versionFile := filepath.Join(binDir, ".version")
	currentVersion := version.Version()
	if data, err := os.ReadFile(versionFile); err == nil {
		if strings.TrimSpace(string(data)) == currentVersion {
			return
		}
	}

	// Extract binaries
	extracted := 0
	for _, name := range bundledBinaries {
		src := filepath.Join(resourcesDir, name)
		if _, err := os.Stat(src); err != nil {
			continue // Binary not in bundle (optional component)
		}
		dst := filepath.Join(binDir, name)
		if err := copyFile(src, dst); err != nil {
			log.Printf("aegis-ui: extract %s: %v", name, err)
			continue
		}
		os.Chmod(dst, 0755)
		extracted++
	}

	// Extract bundled dylibs (libkrun and dependencies) to ~/.aegis/lib/
	frameworksDir := filepath.Join(filepath.Dir(exe), "..", "Frameworks")
	if entries, err := os.ReadDir(frameworksDir); err == nil {
		libDir := filepath.Join(aegisDir(), "lib")
		os.MkdirAll(libDir, 0755)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".dylib") {
				continue
			}
			src := filepath.Join(frameworksDir, e.Name())
			dst := filepath.Join(libDir, e.Name())
			if err := copyFile(src, dst); err != nil {
				log.Printf("aegis-ui: extract lib %s: %v", e.Name(), err)
				continue
			}
			os.Chmod(dst, 0755)
			extracted++
		}
	}

	// Extract kit manifest
	kitSrc := filepath.Join(resourcesDir, "kits", "agent.json")
	if _, err := os.Stat(kitSrc); err == nil {
		kitDir := filepath.Join(aegisDir(), "kits")
		os.MkdirAll(kitDir, 0755)
		copyFile(kitSrc, filepath.Join(kitDir, "agent.json"))
	}

	// Write version marker
	os.WriteFile(versionFile, []byte(currentVersion), 0644)

	if extracted > 0 {
		log.Printf("aegis-ui: extracted %d binaries + libs (version %s)", extracted, currentVersion)
	}
}

// findAegisdBinary locates the aegisd binary.
// Search order:
//  1. ~/.aegis/bin/aegisd (extracted binaries — primary for desktop app)
//  2. Next to this executable (dev mode — all binaries in bin/)
func findAegisdBinary() string {
	// 1. Extracted/installed binaries
	candidate := filepath.Join(aegisDir(), "bin", "aegisd")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// 2. Next to this executable (dev mode)
	exe, _ := os.Executable()
	candidate = filepath.Join(filepath.Dir(exe), "aegisd")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
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
