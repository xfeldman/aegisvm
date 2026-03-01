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

// ensureDesktopSetup installs bundled binaries and config for CLI access.
//
// On macOS: creates symlinks from ~/.aegis/bin/ into the .app bundle.
// On Linux: copies binaries from the AppImage to ~/.aegis/bin/ (FUSE mount is temporary).
//
// No-op when not running from a bundle (dev mode).
func ensureDesktopSetup() {
	switch runtime.GOOS {
	case "darwin":
		ensureDesktopSetupDarwin()
	case "linux":
		ensureDesktopSetupLinux()
	}
}

// ensureDesktopSetupDarwin creates symlinks from ~/.aegis/bin/ into the .app bundle.
// macOS runs binaries directly from the bundle (like Docker Desktop). Only CLI
// and MCP symlinks are created. Dragging the .app to Trash cleanly removes everything.
func ensureDesktopSetupDarwin() {
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

// ensureDesktopSetupLinux copies bundled binaries from the AppImage to ~/.aegis/.
// Unlike macOS (which uses symlinks into a persistent .app bundle), Linux AppImage
// mounts are temporary FUSE filesystems — binaries must be copied to a stable
// location so the daemon survives app exit and the CLI works independently.
func ensureDesktopSetupLinux() {
	exe, err := os.Executable()
	if err != nil {
		return
	}

	// Detect AppImage: $APPDIR is set by the AppImage runtime to the FUSE mount root.
	// Fall back to the directory containing the executable (dev mode).
	appDir := os.Getenv("APPDIR")
	var binSrc, shareSrc string
	if appDir != "" {
		binSrc = filepath.Join(appDir, "usr", "bin")
		shareSrc = filepath.Join(appDir, "usr", "share", "aegisvm")
	} else {
		binSrc = filepath.Dir(exe)
	}

	// Verify we're in a bundle context (aegisd must be present).
	if _, err := os.Stat(filepath.Join(binSrc, "aegisd")); err != nil {
		return // Not in a bundle (dev mode without full binaries)
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

	// Copy runtime binaries to ~/.aegis/bin/.
	// Required: daemon, CLI, harness, Cloud Hypervisor components.
	for _, name := range []string{"aegis", "aegisd", "aegis-mcp", "aegis-harness", "aegis-mcp-guest", "cloud-hypervisor", "ch-remote"} {
		src := filepath.Join(binSrc, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(binDir, name)
		if err := copyFile(src, dst); err != nil {
			log.Printf("aegis-ui: copy %s: %v", name, err)
			continue
		}
		os.Chmod(dst, 0755)
	}

	// Optional: agent kit binaries.
	for _, name := range []string{"aegis-gateway", "aegis-agent"} {
		src := filepath.Join(binSrc, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(binDir, name)
		if err := copyFile(src, dst); err != nil {
			log.Printf("aegis-ui: copy %s: %v", name, err)
			continue
		}
		os.Chmod(dst, 0755)
	}

	// Copy kernel (Cloud Hypervisor needs vmlinux).
	if shareSrc != "" {
		kernelSrc := filepath.Join(shareSrc, "kernel", "vmlinux")
		if _, err := os.Stat(kernelSrc); err == nil {
			kernelDir := filepath.Join(aegisDir(), "kernel")
			os.MkdirAll(kernelDir, 0755)
			if err := copyFile(kernelSrc, filepath.Join(kernelDir, "vmlinux")); err != nil {
				log.Printf("aegis-ui: copy vmlinux: %v", err)
			}
		}
	}

	// Extract kit manifest.
	if shareSrc != "" {
		kitSrc := filepath.Join(shareSrc, "kits", "agent.json")
		if _, err := os.Stat(kitSrc); err == nil {
			kitDir := filepath.Join(aegisDir(), "kits")
			os.MkdirAll(kitDir, 0755)
			copyFile(kitSrc, filepath.Join(kitDir, "agent.json"))
		}
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

	// Some backends (Cloud Hypervisor on Linux) need root.
	// Use pkexec for graphical privilege escalation (polkit dialog),
	// falling back to sudo for terminal contexts.
	platform, _ := config.DetectPlatform()
	needsElevation := platform != nil && platform.NeedsRoot && os.Geteuid() != 0

	var cmd *exec.Cmd
	if needsElevation {
		if _, err := exec.LookPath("pkexec"); err == nil {
			// Pass SUDO_UID/SUDO_GID so aegisd's chownToInvokingUser() can
			// fix ownership of ~/.aegis/ (socket, DB, PID) for the non-root user.
			uid := strconv.Itoa(os.Getuid())
			gid := strconv.Itoa(os.Getgid())
			cmd = exec.Command("pkexec", "--disable-internal-agent",
				"env",
				"HOME="+os.Getenv("HOME"),
				"SUDO_UID="+uid,
				"SUDO_GID="+gid,
				aegisdBin)
		} else {
			cmd = exec.Command("sudo", "--preserve-env=HOME", aegisdBin)
		}
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

	// Monitor for early exit (or pkexec dialog cancel).
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// Wait for daemon to be ready.
	// With pkexec, the user needs time to enter their password — wait up to 60s.
	// Without elevation, the daemon should start within ~2.5s.
	maxAttempts := 10 // ~2.5s for direct start
	if needsElevation {
		maxAttempts = 300 // ~60s for pkexec dialog
	}

	time.Sleep(500 * time.Millisecond)
	for i := 0; i < maxAttempts; i++ {
		if isDaemonRunning() {
			log.Println("aegis-ui: aegisd started")
			return
		}
		select {
		case <-exited:
			log.Println("aegis-ui: aegisd exited — check ~/.aegis/data/aegisd.log")
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
