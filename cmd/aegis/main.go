// aegis is the CLI for the Aegis agent runtime platform.
//
// Commands:
//
//	aegis up                   Start aegisd daemon
//	aegis down                 Stop aegisd daemon
//	aegis run                  Run a command in an ephemeral microVM (start + follow + delete)
//	aegis instance start       Start new or restart stopped instance
//	aegis instance list        List instances (--stopped, --running)
//	aegis instance info        Show instance details
//	aegis instance stop        Stop an instance (keep record)
//	aegis instance delete      Delete an instance (remove entirely)
//	aegis instance pause       Pause an instance
//	aegis instance resume      Resume a paused instance
//	aegis instance prune       Remove stale stopped instances
//	aegis exec                 Execute a command in a running instance
//	aegis logs                 Stream instance logs
//	aegis secret set           Set a secret
//	aegis secret list          List secrets
//	aegis secret delete        Delete a secret
//	aegis mcp install          Register aegis-mcp in Claude Code
//	aegis mcp uninstall        Remove aegis-mcp from Claude Code
//	aegis status               Show daemon status
//	aegis doctor               Print platform and backend info
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "up":
		cmdUp()
	case "down":
		cmdDown()
	case "run":
		cmdRun()
	case "status":
		cmdStatus()
	case "doctor":
		cmdDoctor()
	case "instance":
		cmdInstance()
	case "exec":
		cmdExec()
	case "logs":
		cmdLogs()
	case "secret":
		cmdSecret()
	case "mcp":
		cmdMCP()
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Usage: aegis <command> [options]

Commands:
  up         Start aegisd daemon
  down       Stop aegisd daemon
  run        Run a command in an ephemeral microVM (start + follow + delete)
  status     Show daemon status
  doctor     Print platform and backend info
  instance   Manage instances (start, list, info, stop, delete, pause, resume, prune)
  exec       Execute a command in a running instance
  logs       Stream instance logs
  secret     Manage secrets (set, list, delete)
  mcp        MCP server management (install)

Examples:
  aegis up
  aegis run -- echo "hello from aegisvm"
  aegis run --expose 80 -- python3 -m http.server 80
  aegis run --expose 8080:80 -- python3 -m http.server 80
  aegis run --workspace ./myapp --expose 80 -- python3 /workspace/app.py
  aegis instance start --name web --expose 8080:80 --workspace myapp -- python3 -m http.server 80
  aegis instance stop web
  aegis instance start --name web                                    (restart stopped)
  aegis instance list --stopped
  aegis instance prune --stopped-older-than 7d
  aegis exec web -- echo hello
  aegis logs web --follow
  aegis secret set API_KEY sk-test123
  aegis mcp install
  aegis down`)
}

func socketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "aegisd.sock")
}

func pidFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "data", "aegisd.pid")
}

func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath(), 5*time.Second)
			},
		},
		Timeout: 0, // No timeout for streaming
	}
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
	return err == nil
}

func cmdUp() {
	if isDaemonRunning() {
		fmt.Println("aegisd is already running")
		return
	}

	exe, _ := os.Executable()
	aegisdBin := filepath.Join(filepath.Dir(exe), "aegisd")
	if _, err := os.Stat(aegisdBin); err != nil {
		fmt.Fprintf(os.Stderr, "aegisd binary not found at %s\n", aegisdBin)
		os.Exit(1)
	}

	// Redirect daemon output to log file instead of terminal
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".aegis", "data")
	os.MkdirAll(logDir, 0755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "aegisd.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create log file: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(aegisdBin)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start aegisd: %v\n", err)
		os.Exit(1)
	}

	time.Sleep(500 * time.Millisecond)

	for i := 0; i < 10; i++ {
		if isDaemonRunning() {
			fmt.Printf("aegisd started (pid %d)\n", cmd.Process.Pid)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Fprintln(os.Stderr, "aegisd did not start within timeout")
	os.Exit(1)
}

func cmdDown() {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		fmt.Println("aegisd is not running")
		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		fmt.Println("aegisd is not running (invalid pid file)")
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Println("aegisd is not running")
		return
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "send SIGTERM: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("aegisd stopping (pid %d)\n", pid)

	for i := 0; i < 50; i++ {
		if !isDaemonRunning() {
			fmt.Println("aegisd stopped")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Fprintln(os.Stderr, "aegisd did not stop within timeout")
	os.Exit(1)
}

type exposeFlag struct {
	GuestPort  int
	PublicPort int    // 0 = random
	Protocol   string // "http", "tcp", etc. Default: "http"
}

// parseRunFlags parses common flags: --expose, --name, --env, --image, --workspace, --secret
func parseRunFlags(args []string) (exposePorts []exposeFlag, name, imageRef string, envVars map[string]string, secretKeys []string, workspace string, command []string) {
	envVars = make(map[string]string)

	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			command = args[i+1:]
			break
		}
		switch args[i] {
		case "--expose":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--expose requires a port (e.g. 80, 8080:80, 8080:80/tcp)")
				os.Exit(1)
			}
			exposePorts = append(exposePorts, parseExposeArg(args[i+1]))
			i++
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--name requires a value")
				os.Exit(1)
			}
			name = args[i+1]
			i++
		case "--image":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--image requires a value")
				os.Exit(1)
			}
			imageRef = args[i+1]
			i++
		case "--env":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--env requires KEY=VALUE")
				os.Exit(1)
			}
			kv := args[i+1]
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				fmt.Fprintf(os.Stderr, "invalid --env format: %s (expected KEY=VALUE)\n", kv)
				os.Exit(1)
			}
			envVars[kv[:eq]] = kv[eq+1:]
			i++
		case "--secret":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--secret requires a key name (or '*' for all)")
				os.Exit(1)
			}
			secretKeys = append(secretKeys, args[i+1])
			i++
		case "--workspace":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--workspace requires a path")
				os.Exit(1)
			}
			workspace = args[i+1]
			i++
		}
	}
	return
}

// parseExposeArg parses --expose argument in Docker-style format:
//
//	80          → random public port → guest 80
//	8080:80     → public 8080 → guest 80
//	8080:80/tcp → public 8080 → guest 80, protocol tcp
//	80/tcp      → random public port → guest 80, protocol tcp
func parseExposeArg(arg string) exposeFlag {
	proto := "http"
	portPart := arg

	// Extract protocol suffix: /http, /tcp, /grpc
	if idx := strings.LastIndexByte(arg, '/'); idx >= 0 {
		proto = arg[idx+1:]
		portPart = arg[:idx]
	}

	// Check for public:guest format
	if idx := strings.IndexByte(portPart, ':'); idx >= 0 {
		publicStr := portPart[:idx]
		guestStr := portPart[idx+1:]
		publicPort, err := strconv.Atoi(publicStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid public port: %s\n", arg)
			os.Exit(1)
		}
		guestPort, err := strconv.Atoi(guestStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid guest port: %s\n", arg)
			os.Exit(1)
		}
		return exposeFlag{GuestPort: guestPort, PublicPort: publicPort, Protocol: proto}
	}

	// Just a guest port
	guestPort, err := strconv.Atoi(portPart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid port: %s\n", arg)
		os.Exit(1)
	}
	return exposeFlag{GuestPort: guestPort, PublicPort: 0, Protocol: proto}
}

// cmdRun creates an ephemeral instance: start → follow logs → wait → delete.
// If --workspace is omitted, a temporary workspace is allocated and deleted after.
// If --workspace is provided, that workspace is preserved (user-owned).
func cmdRun() {
	args := os.Args[2:]

	exposePorts, name, imageRef, envVars, secretKeys, workspace, command := parseRunFlags(args)

	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aegis run [--expose PORT] [--name NAME] [--env K=V] [--secret KEY] [--image IMAGE] -- COMMAND [args...]")
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	// Default to python:3.12-alpine if no --image and no base-rootfs
	if imageRef == "" {
		if _, err := os.Stat(baseRootfsPath()); os.IsNotExist(err) {
			imageRef = defaultImage
		}
	}

	// If no workspace provided, allocate a temporary named workspace
	tempWorkspace := ""
	if workspace == "" {
		tempWorkspace = fmt.Sprintf("run-%d", time.Now().UnixNano())
		workspace = tempWorkspace
	}

	client := httpClient()

	// Build create instance request
	reqBody := map[string]interface{}{
		"command":   command,
		"workspace": workspace,
	}
	if len(exposePorts) > 0 {
		exposes := make([]map[string]interface{}, len(exposePorts))
		for i, p := range exposePorts {
			expose := map[string]interface{}{"port": p.GuestPort, "protocol": p.Protocol}
			if p.PublicPort > 0 {
				expose["public_port"] = p.PublicPort
			}
			exposes[i] = expose
		}
		reqBody["exposes"] = exposes
	}
	if name != "" {
		reqBody["handle"] = name
	}
	if imageRef != "" {
		reqBody["image_ref"] = imageRef
	}
	if len(envVars) > 0 {
		reqBody["env"] = envVars
	}
	if len(secretKeys) > 0 {
		reqBody["secrets"] = secretKeys
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post("http://aegis/v1/instances", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "create instance failed (%d): %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	var inst map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&inst)
	instanceID := inst["id"].(string)

	if len(exposePorts) > 0 {
		routerAddr, _ := inst["router_addr"].(string)
		fmt.Printf("Serving on http://%s\n", routerAddr)
	}

	// Set up signal handler for cleanup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	exitCode := 0
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Follow logs. The logstore entry is pre-created at instance creation,
		// so follow connects to a real subscriber even before boot starts.
		logsResp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s/logs?follow=1", instanceID))
		if err != nil {
			fmt.Fprintf(os.Stderr, "follow logs: %v\n", err)
			return
		}
		defer logsResp.Body.Close()

		scanner := bufio.NewScanner(logsResp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var entry map[string]interface{}
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}

			source, _ := entry["source"].(string)
			text, _ := entry["line"].(string)
			if source == "system" && strings.HasPrefix(text, "process exited") {
				if idx := strings.Index(text, "code="); idx >= 0 {
					codeStr := text[idx+5:]
					codeStr = strings.TrimSuffix(codeStr, ")")
					if ec, err := strconv.Atoi(codeStr); err == nil {
						exitCode = ec
					}
				}
				return
			}

			printLogEntry(entry)
		}
	}()

	// Wait for either logs to finish or signal
	select {
	case <-done:
		// Process exited naturally
	case <-sigCh:
		fmt.Println("\nStopping instance...")
	}

	// Clean up: delete the instance
	delReq, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instanceID), nil)
	client.Do(delReq)

	// Clean up temp workspace
	if tempWorkspace != "" {
		home, _ := os.UserHomeDir()
		wsPath := filepath.Join(home, ".aegis", "data", "workspaces", tempWorkspace)
		os.RemoveAll(wsPath)
	}

	os.Exit(exitCode)
}

func cmdStatus() {
	if !isDaemonRunning() {
		fmt.Println("aegisd: not running")
		return
	}

	client := httpClient()
	resp, err := client.Get("http://aegis/v1/status")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get status: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var status map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&status)

	fmt.Printf("aegisd: %s\n", status["status"])
	fmt.Printf("backend: %s\n", status["backend"])
}

func cmdDoctor() {
	fmt.Println("Aegis Doctor")
	fmt.Println("============")
	fmt.Println()

	fmt.Printf("Go:       installed\n")

	_, err := exec.LookPath("krunvm")
	if err == nil {
		fmt.Printf("krunvm:   found (libkrun CLI available)\n")
	} else {
		fmt.Printf("krunvm:   not found\n")
	}

	libPaths := []string{
		"/opt/homebrew/lib/libkrun.dylib",
		"/usr/local/lib/libkrun.dylib",
		"/usr/lib/libkrun.so",
	}
	libFound := false
	for _, p := range libPaths {
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("libkrun:  found at %s\n", p)
			libFound = true
			break
		}
	}
	if !libFound {
		fmt.Printf("libkrun:  not found (install via: brew tap slp/krun && brew install libkrun)\n")
	}

	_, err = exec.LookPath("mkfs.ext4")
	if err == nil {
		fmt.Printf("e2fsprogs: found\n")
	} else {
		fmt.Printf("e2fsprogs: not found (install via: brew install e2fsprogs)\n")
	}

	fmt.Println()
	if isDaemonRunning() {
		fmt.Printf("aegisd:   running\n")

		client := httpClient()
		resp, err := client.Get("http://aegis/v1/status")
		if err == nil {
			defer resp.Body.Close()
			var status map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&status)

			if backend, ok := status["backend"].(string); ok {
				fmt.Printf("\nBackend:     %s\n", backend)
			}

			if caps, ok := status["capabilities"].(map[string]interface{}); ok {
				fmt.Println("Capabilities:")
				if v, ok := caps["pause_resume"].(bool); ok {
					fmt.Printf("  Pause/Resume:          %s\n", boolYesNo(v))
				}
				if v, ok := caps["boot_from_disk_layers"].(bool); ok {
					fmt.Printf("  Boot from disk layers: %s\n", boolYesNo(v))
				}
			}
		}
	} else {
		fmt.Printf("aegisd:   not running\n")
	}
}

func boolYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// cmdInstance dispatches instance subcommands.
func cmdInstance() {
	if len(os.Args) < 3 {
		instanceUsage()
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	switch os.Args[2] {
	case "start":
		cmdInstanceStart(client)
	case "list":
		cmdInstanceList(client)
	case "info":
		cmdInstanceInfo(client)
	case "stop":
		cmdInstanceStop(client)
	case "delete":
		cmdInstanceDelete(client)
	case "pause":
		cmdInstancePause(client)
	case "resume":
		cmdInstanceResume(client)
	case "prune":
		cmdInstancePrune(client)
	case "help", "--help", "-h":
		instanceUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown instance command: %s\n", os.Args[2])
		instanceUsage()
		os.Exit(1)
	}
}

func instanceUsage() {
	fmt.Println(`Usage: aegis instance <command> [options]

Commands:
  start    Start a new instance (or restart a stopped instance by --name)
  list     List instances (--stopped, --running to filter)
  info     Show instance details
  stop     Stop an instance (VM stopped, record kept for restart)
  delete   Delete an instance (removed entirely, logs cleaned)
  pause    Pause a running instance (SIGSTOP)
  resume   Resume a paused instance (SIGCONT)
  prune    Remove stopped instances older than a threshold

Examples:
  aegis instance start --name web --secret API_KEY --expose 80 -- python3 -m http.server 80
  aegis instance start --name web                                (restart stopped instance)
  aegis instance list
  aegis instance list --stopped
  aegis instance info web
  aegis instance stop web
  aegis instance delete web
  aegis instance prune --stopped-older-than 7d`)
}

// cmdInstanceStart starts a new instance.
func cmdInstanceStart(client *http.Client) {
	args := os.Args[3:]

	exposePorts, name, imageRef, envVars, secretKeys, workspace, command := parseRunFlags(args)

	if len(command) == 0 && name == "" {
		fmt.Fprintln(os.Stderr, "usage: aegis instance start [--name NAME] [--expose PORT] [--env K=V] [--secret KEY] [--image IMAGE] -- COMMAND [args...]")
		fmt.Fprintln(os.Stderr, "       aegis instance start --name NAME   (restart stopped instance)")
		os.Exit(1)
	}

	// Default to python:3.12-alpine if no --image and no base-rootfs
	if imageRef == "" && len(command) > 0 {
		if _, err := os.Stat(baseRootfsPath()); os.IsNotExist(err) {
			imageRef = defaultImage
		}
	}

	reqBody := map[string]interface{}{}
	if len(command) > 0 {
		reqBody["command"] = command
	}
	if len(exposePorts) > 0 {
		exposes := make([]map[string]interface{}, len(exposePorts))
		for i, p := range exposePorts {
			expose := map[string]interface{}{"port": p.GuestPort, "protocol": p.Protocol}
			if p.PublicPort > 0 {
				expose["public_port"] = p.PublicPort
			}
			exposes[i] = expose
		}
		reqBody["exposes"] = exposes
	}
	if name != "" {
		reqBody["handle"] = name
	}
	if imageRef != "" {
		reqBody["image_ref"] = imageRef
	}
	if len(envVars) > 0 {
		reqBody["env"] = envVars
	}
	if len(secretKeys) > 0 {
		reqBody["secrets"] = secretKeys
	}
	if workspace != "" {
		reqBody["workspace"] = workspace
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post("http://aegis/v1/instances", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "start instance failed (%d): %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	var inst map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&inst)

	id, _ := inst["id"].(string)
	handle, _ := inst["handle"].(string)
	routerAddr, _ := inst["router_addr"].(string)

	if resp.StatusCode == http.StatusOK {
		fmt.Printf("Instance restarted: %s\n", id)
	} else {
		fmt.Printf("Instance started: %s\n", id)
	}
	if handle != "" {
		fmt.Printf("Handle: %s\n", handle)
	}
	if routerAddr != "" {
		fmt.Printf("Router: http://%s\n", routerAddr)
	}
}

func cmdInstanceList(client *http.Client) {
	// Parse optional --stopped or --running filter
	url := "http://aegis/v1/instances"
	for _, arg := range os.Args[3:] {
		switch arg {
		case "--stopped":
			url = "http://aegis/v1/instances?state=stopped"
		case "--running":
			url = "http://aegis/v1/instances?state=running"
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list instances: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var instances []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&instances)

	if len(instances) == 0 {
		fmt.Println("No instances")
		return
	}

	fmt.Printf("%-30s %-15s %-10s %-20s\n", "ID", "HANDLE", "STATE", "STOPPED AT")
	for _, inst := range instances {
		id, _ := inst["id"].(string)
		state, _ := inst["state"].(string)
		handle, _ := inst["handle"].(string)
		stoppedAt, _ := inst["stopped_at"].(string)
		if handle == "" {
			handle = "-"
		}
		if stoppedAt == "" {
			stoppedAt = "-"
		}
		fmt.Printf("%-30s %-15s %-10s %-20s\n", id, handle, state, stoppedAt)
	}
}

func cmdInstanceInfo(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance info HANDLE_OR_ID")
		os.Exit(1)
	}

	target := os.Args[3]
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", instID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	var inst map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&inst)

	fmt.Printf("ID:          %s\n", inst["id"])
	fmt.Printf("State:       %s\n", inst["state"])
	if handle, ok := inst["handle"].(string); ok && handle != "" {
		fmt.Printf("Handle:      %s\n", handle)
	}
	if imageRef, ok := inst["image_ref"].(string); ok && imageRef != "" {
		fmt.Printf("Image:       %s\n", imageRef)
	}
	if cmd, ok := inst["command"].([]interface{}); ok && len(cmd) > 0 {
		parts := make([]string, len(cmd))
		for i, c := range cmd {
			parts[i] = fmt.Sprint(c)
		}
		fmt.Printf("Command:     %s\n", strings.Join(parts, " "))
	}
	if ports, ok := inst["expose_ports"].([]interface{}); ok && len(ports) > 0 {
		parts := make([]string, len(ports))
		for i, p := range ports {
			parts[i] = fmt.Sprint(p)
		}
		fmt.Printf("Ports:       %s\n", strings.Join(parts, ", "))
	}
	if ra, ok := inst["router_addr"].(string); ok && ra != "" {
		fmt.Printf("Router:      http://%s\n", ra)
	}
	if eps, ok := inst["endpoints"].([]interface{}); ok && len(eps) > 0 {
		fmt.Println("Endpoints:")
		for _, ep := range eps {
			if epm, ok := ep.(map[string]interface{}); ok {
				publicPort := epm["public_port"]
				if publicPort == nil {
					publicPort = epm["host_port"] // backward compat
				}
				fmt.Printf("  :%v → :%v (%v)\n", epm["guest_port"], publicPort, epm["protocol"])
			}
		}
	}
	if conns, ok := inst["active_connections"].(float64); ok {
		fmt.Printf("Connections: %.0f\n", conns)
	}
	if createdAt, ok := inst["created_at"].(string); ok {
		fmt.Printf("Created:     %s\n", createdAt)
	}
	if lastActive, ok := inst["last_active_at"].(string); ok {
		fmt.Printf("Last Active: %s\n", lastActive)
	}
}

func cmdInstanceStop(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance stop HANDLE_OR_ID")
		os.Exit(1)
	}

	target := os.Args[3]
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	resp, err := client.Post(fmt.Sprintf("http://aegis/v1/instances/%s/stop", instID), "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "stop failed: %s\n", body)
		os.Exit(1)
	}

	fmt.Printf("Instance %s stopped\n", target)
}

func cmdInstanceDelete(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance delete HANDLE_OR_ID")
		os.Exit(1)
	}

	target := os.Args[3]
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instID), nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "delete failed: %s\n", body)
		os.Exit(1)
	}

	fmt.Printf("Instance %s deleted\n", target)
}

func cmdInstancePause(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance pause HANDLE_OR_ID")
		os.Exit(1)
	}

	target := os.Args[3]
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	resp, err := client.Post(fmt.Sprintf("http://aegis/v1/instances/%s/pause", instID), "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pause instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "pause failed: %s\n", body)
		os.Exit(1)
	}

	fmt.Printf("Instance %s paused\n", target)
}

func cmdInstanceResume(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance resume HANDLE_OR_ID")
		os.Exit(1)
	}

	target := os.Args[3]
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", target)
		os.Exit(1)
	}

	resp, err := client.Post(fmt.Sprintf("http://aegis/v1/instances/%s/resume", instID), "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resume instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "resume failed: %s\n", body)
		os.Exit(1)
	}

	fmt.Printf("Instance %s resumed\n", target)
}

func cmdInstancePrune(client *http.Client) {
	olderThan := "7d" // default
	for i := 3; i < len(os.Args); i++ {
		if os.Args[i] == "--stopped-older-than" && i+1 < len(os.Args) {
			olderThan = os.Args[i+1]
			i++
		}
	}

	resp, err := client.Post(fmt.Sprintf("http://aegis/v1/instances/prune?older_than=%s", olderThan), "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prune instances: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "prune failed: %s\n", body)
		os.Exit(1)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	pruned, _ := result["pruned"].(float64)
	fmt.Printf("Pruned %d stopped instance(s)\n", int(pruned))
}

// cmdExec executes a command in a running instance.
func cmdExec() {
	args := os.Args[2:]

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aegis exec HANDLE_OR_ID -- COMMAND [args...]")
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	target := args[0]
	var command []string
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			command = args[i+1:]
			break
		}
	}

	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aegis exec HANDLE_OR_ID -- COMMAND [args...]")
		os.Exit(1)
	}

	client := httpClient()

	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "could not resolve %q to an instance\n", target)
		os.Exit(1)
	}

	reqBody := map[string]interface{}{
		"command": command,
	}
	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/instances/%s/exec", instID),
		"application/json",
		bytes.NewReader(bodyJSON),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		fmt.Fprintln(os.Stderr, "instance is stopped")
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "exec failed (%d): %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	decoder := json.NewDecoder(resp.Body)
	first := true
	for decoder.More() {
		var entry map[string]interface{}
		if err := decoder.Decode(&entry); err != nil {
			break
		}

		// First line is exec info header
		if first {
			first = false
			continue
		}

		// Done marker
		if done, _ := entry["done"].(bool); done {
			if ec, ok := entry["exit_code"].(float64); ok && int(ec) != 0 {
				os.Exit(int(ec))
			}
			return
		}

		line, _ := entry["line"].(string)
		stream, _ := entry["stream"].(string)
		if stream == "stderr" {
			fmt.Fprintln(os.Stderr, line)
		} else {
			fmt.Println(line)
		}
	}
}

// resolveInstanceTarget resolves a target (handle or instance ID) to an instance ID.
func resolveInstanceTarget(client *http.Client, target string) string {
	// Try as instance ID first
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", target))
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var inst map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&inst)
			id, _ := inst["id"].(string)
			return id
		}
	}

	// Try as handle — scan instances list
	instResp, err := client.Get("http://aegis/v1/instances")
	if err != nil {
		return ""
	}
	defer instResp.Body.Close()

	var instances []map[string]interface{}
	json.NewDecoder(instResp.Body).Decode(&instances)

	for _, inst := range instances {
		handle, _ := inst["handle"].(string)
		if handle == target {
			id, _ := inst["id"].(string)
			return id
		}
	}
	return ""
}

// cmdLogs streams logs for an instance.
func cmdLogs() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: aegis logs HANDLE_OR_ID [--follow]")
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	target := os.Args[2]
	follow := false
	for _, arg := range os.Args[3:] {
		if arg == "--follow" || arg == "-f" {
			follow = true
		}
	}

	client := httpClient()

	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "no instance found for %q\n", target)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://aegis/v1/instances/%s/logs", instID)
	if follow {
		url += "?follow=1"
	}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get logs: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var entry map[string]interface{}
		if err := decoder.Decode(&entry); err != nil {
			break
		}
		printLogEntry(entry)
	}
}

// ANSI color codes for log sources.
const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[36m"   // exec: cyan
	colorYellow = "\033[33m"   // system: yellow
	// server: no color (default terminal color)
)

// printLogEntry formats and prints a log entry with color-coded [source] prefix.
func printLogEntry(entry map[string]interface{}) {
	line, _ := entry["line"].(string)
	stream, _ := entry["stream"].(string)
	source, _ := entry["source"].(string)

	var prefix string
	switch source {
	case "exec":
		prefix = colorCyan + "[exec]" + colorReset + " "
	case "system":
		prefix = colorYellow + "[system]" + colorReset + " "
	default:
		prefix = ""
	}

	switch stream {
	case "stderr":
		fmt.Fprintf(os.Stderr, "%s%s\n", prefix, line)
	default:
		fmt.Printf("%s%s\n", prefix, line)
	}
}

// cmdSecret dispatches secret subcommands.
func cmdSecret() {
	if len(os.Args) < 3 {
		secretUsage()
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	switch os.Args[2] {
	case "set":
		cmdSecretSet(client)
	case "list":
		cmdSecretList(client)
	case "delete":
		cmdSecretDelete(client)
	case "help", "--help", "-h":
		secretUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown secret command: %s\n", os.Args[2])
		secretUsage()
		os.Exit(1)
	}
}

func secretUsage() {
	fmt.Println(`Usage: aegis secret <command> [options]

Commands:
  set      Set a secret
  list     List secret names
  delete   Delete a secret

Examples:
  aegis secret set API_KEY sk-test123
  aegis secret list
  aegis secret delete API_KEY`)
}

// cmdSecretSet sets a workspace secret.
// aegis secret set <key> <value>
func cmdSecretSet(client *http.Client) {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret set KEY VALUE")
		os.Exit(1)
	}

	key := os.Args[3]
	value := os.Args[4]

	bodyJSON, _ := json.Marshal(map[string]string{"value": value})
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://aegis/v1/secrets/%s", key),
		bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set secret: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "set secret failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Secret %s set\n", key)
}

// cmdSecretList lists workspace secrets.
func cmdSecretList(client *http.Client) {
	resp, err := client.Get("http://aegis/v1/secrets")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list secrets: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var secrets []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&secrets)

	if len(secrets) == 0 {
		fmt.Println("No secrets")
		return
	}

	fmt.Println("Secrets:")
	for _, sec := range secrets {
		name, _ := sec["name"].(string)
		fmt.Printf("  %s\n", name)
	}
}

func cmdSecretDelete(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret delete KEY")
		os.Exit(1)
	}

	key := os.Args[3]

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/secrets/%s", key), nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete secret: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "delete secret failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Secret %s deleted\n", key)
}

// --- MCP ---

func cmdMCP() {
	if len(os.Args) < 3 {
		fmt.Println(`Usage: aegis mcp <command>

Commands:
  install    Register aegis-mcp as an MCP server in Claude Code
  uninstall  Remove aegis-mcp from Claude Code`)
		os.Exit(1)
	}

	switch os.Args[2] {
	case "install":
		cmdMCPInstall()
	case "uninstall":
		cmdMCPUninstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func findMCPBinary() string {
	// Look next to our own binary first
	exe, _ := os.Executable()
	candidate := filepath.Join(filepath.Dir(exe), "aegis-mcp")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Fall back to PATH
	if p, err := exec.LookPath("aegis-mcp"); err == nil {
		return p
	}

	return ""
}

func cmdMCPInstall() {
	mcpBin := findMCPBinary()
	if mcpBin == "" {
		fmt.Fprintln(os.Stderr, "aegis-mcp binary not found (not next to aegis and not in PATH)")
		os.Exit(1)
	}

	// Check that claude CLI exists
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude CLI not found in PATH — install Claude Code first")
		fmt.Fprintln(os.Stderr, "  https://docs.anthropic.com/en/docs/claude-code")
		os.Exit(1)
	}

	// Determine scope
	scope := "user"
	for _, arg := range os.Args[3:] {
		if arg == "--project" {
			scope = "project"
		}
	}

	cmd := exec.Command(claudeBin, "mcp", "add", "--transport", "stdio", "--scope", scope, "aegis", "--", mcpBin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to register MCP server: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("aegis MCP server registered in Claude Code")
	fmt.Printf("  binary: %s\n", mcpBin)
	fmt.Printf("  scope:  %s\n", scope)
}

func cmdMCPUninstall() {
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude CLI not found in PATH")
		os.Exit(1)
	}

	cmd := exec.Command(claudeBin, "mcp", "remove", "aegis")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to remove MCP server: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("aegis MCP server removed from Claude Code")
}

// --- Default image ---
//
// When no --image is specified and no base-rootfs exists, the CLI
// automatically sets image_ref to defaultImage. aegisd's existing
// OCI pull/cache mechanism handles the rest — no separate rootfs
// download infrastructure needed.

const defaultImage = "python:3.12-alpine"

func baseRootfsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "base-rootfs")
}
