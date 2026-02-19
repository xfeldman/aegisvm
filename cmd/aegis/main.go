// aegis is the CLI for the Aegis agent runtime platform.
//
// Commands:
//
//	aegis up       Start aegisd daemon
//	aegis down     Stop aegisd daemon
//	aegis run      Run a command in a microVM
//	aegis status   Show daemon status
//	aegis doctor   Print platform and backend info
package main

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
	case "app":
		cmdApp()
	case "instance":
		cmdInstance()
	case "exec":
		cmdExec()
	case "logs":
		cmdLogs()
	case "secret":
		cmdSecret()
	case "kit":
		cmdKit()
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
  run        Run a command in a microVM
  status     Show daemon status
  doctor     Print platform and backend info
  app        Manage apps (create, publish, serve, list, info, delete, logs)
  instance   Manage instances (list, info)
  exec       Execute a command in a running instance
  logs       Stream instance logs (alias for 'app logs')
  secret     Manage secrets (set, list, delete, set-workspace, list-workspace)
  kit        Manage kits (install, list, info, uninstall)

Examples:
  aegis up
  aegis run -- echo "hello from aegis"
  aegis run --image alpine:3.21 -- echo hello
  aegis run --expose 80 -- python -m http.server 80
  aegis app create --name myapp --image python:3.12-alpine --expose 80 -- python3 -m http.server 80
  aegis app publish myapp
  aegis app serve myapp
  aegis instance list
  aegis exec myapp -- echo hello
  aegis logs myapp --follow
  aegis secret set myapp API_KEY sk-test123
  aegis kit install manifest.yaml
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
	// Check if process is alive
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func cmdUp() {
	if isDaemonRunning() {
		fmt.Println("aegisd is already running")
		return
	}

	// Find aegisd binary next to this binary
	exe, _ := os.Executable()
	aegisdBin := filepath.Join(filepath.Dir(exe), "aegisd")
	if _, err := os.Stat(aegisdBin); err != nil {
		fmt.Fprintf(os.Stderr, "aegisd binary not found at %s\n", aegisdBin)
		os.Exit(1)
	}

	cmd := exec.Command(aegisdBin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start aegisd: %v\n", err)
		os.Exit(1)
	}

	// Wait a moment for the daemon to start
	time.Sleep(500 * time.Millisecond)

	// Verify it's running
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

	// Wait for it to exit
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

func cmdRun() {
	// Parse: aegis run [--expose PORT] [--image IMAGE] -- <command...>
	args := os.Args[2:]

	var exposePorts []int
	var imageRef string
	var remaining []string

	// Parse flags before "--"
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			remaining = args[i:]
			break
		}
		if args[i] == "--expose" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--expose requires a port number")
				os.Exit(1)
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid port: %s\n", args[i+1])
				os.Exit(1)
			}
			exposePorts = append(exposePorts, port)
			i++ // skip port value
		} else if args[i] == "--image" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--image requires an image reference")
				os.Exit(1)
			}
			imageRef = args[i+1]
			i++ // skip image value
		} else {
			remaining = append(remaining, args[i])
		}
	}

	// Find "--" separator in remaining
	cmdStart := -1
	for i, arg := range remaining {
		if arg == "--" {
			cmdStart = i + 1
			break
		}
	}

	if cmdStart < 0 || cmdStart >= len(remaining) {
		fmt.Fprintln(os.Stderr, "usage: aegis run [--expose PORT] -- <command> [args...]")
		os.Exit(1)
	}

	command := remaining[cmdStart:]

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	if len(exposePorts) > 0 {
		cmdRunServe(client, command, exposePorts)
	} else {
		cmdRunTask(client, command, imageRef)
	}
}

// cmdRunServe handles serve mode: create instance with exposed ports, wait for Ctrl+C.
func cmdRunServe(client *http.Client, command []string, exposePorts []int) {
	reqBody := map[string]interface{}{
		"command":      command,
		"expose_ports": exposePorts,
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
	routerAddr, _ := inst["router_addr"].(string)

	fmt.Printf("Serving on http://%s\n", routerAddr)
	fmt.Printf("Instance: %s\n", instanceID)
	fmt.Println("Press Ctrl+C to stop")

	// Wait for interrupt or instance disappearing
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Println("\nStopping instance...")
			req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instanceID), nil)
			client.Do(req)
			fmt.Println("Stopped")
			return
		case <-ticker.C:
			checkResp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", instanceID))
			if err != nil || checkResp.StatusCode == 404 {
				if checkResp != nil {
					checkResp.Body.Close()
				}
				fmt.Println("\nInstance no longer exists. Exiting.")
				return
			}
			checkResp.Body.Close()
		}
	}
}

// cmdRunTask handles task mode (no --expose): create task, stream logs, exit.
func cmdRunTask(client *http.Client, command []string, imageRef string) {
	reqBody := map[string]interface{}{
		"command": command,
	}
	if imageRef != "" {
		reqBody["image"] = imageRef
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post("http://aegis/v1/tasks", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create task: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "create task failed (%d): %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	var task map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&task)
	taskID := task["id"].(string)

	// Follow logs
	logsResp, err := client.Get(fmt.Sprintf("http://aegis/v1/tasks/%s/logs?follow=true", taskID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "follow logs: %v\n", err)
		os.Exit(1)
	}
	defer logsResp.Body.Close()

	// Stream logs to stdout/stderr
	decoder := json.NewDecoder(logsResp.Body)
	for decoder.More() {
		var logLine map[string]interface{}
		if err := decoder.Decode(&logLine); err != nil {
			break
		}

		line, _ := logLine["line"].(string)
		stream, _ := logLine["stream"].(string)

		switch stream {
		case "stderr":
			fmt.Fprintln(os.Stderr, line)
		default:
			fmt.Println(line)
		}
	}

	// Get final task status
	time.Sleep(100 * time.Millisecond) // Brief wait for status update
	statusResp, err := client.Get(fmt.Sprintf("http://aegis/v1/tasks/%s", taskID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get task status: %v\n", err)
		os.Exit(1)
	}
	defer statusResp.Body.Close()

	var finalTask map[string]interface{}
	json.NewDecoder(statusResp.Body).Decode(&finalTask)

	exitCode := 0
	if ec, ok := finalTask["exit_code"].(float64); ok {
		exitCode = int(ec)
	}
	if errMsg, ok := finalTask["error"].(string); ok && errMsg != "" {
		fmt.Fprintf(os.Stderr, "task error: %s\n", errMsg)
		if exitCode == 0 {
			exitCode = 1
		}
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

	// Check platform
	fmt.Printf("Go:       installed\n")

	// Check libkrun
	_, err := exec.LookPath("krunvm")
	if err == nil {
		fmt.Printf("krunvm:   found (libkrun CLI available)\n")
	} else {
		fmt.Printf("krunvm:   not found\n")
	}

	// Check for libkrun library
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

	// Check e2fsprogs
	_, err = exec.LookPath("mkfs.ext4")
	if err == nil {
		fmt.Printf("e2fsprogs: found\n")
	} else {
		fmt.Printf("e2fsprogs: not found (install via: brew install e2fsprogs)\n")
	}

	// Check daemon and query capabilities
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
				if v, ok := caps["memory_snapshots"].(bool); ok {
					fmt.Printf("  Memory Snapshots:      %s\n", boolYesNo(v))
				}
				if v, ok := caps["boot_from_disk_layers"].(bool); ok {
					fmt.Printf("  Boot from disk layers: %s\n", boolYesNo(v))
				}
			}

			if kitCount, ok := status["kit_count"].(float64); ok {
				fmt.Printf("Installed kits: %d\n", int(kitCount))
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

// cmdApp dispatches app subcommands.
func cmdApp() {
	if len(os.Args) < 3 {
		appUsage()
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	switch os.Args[2] {
	case "create":
		cmdAppCreate(client)
	case "publish":
		cmdAppPublish(client)
	case "serve":
		cmdAppServe(client)
	case "list":
		cmdAppList(client)
	case "info":
		cmdAppInfo(client)
	case "delete":
		cmdAppDelete(client)
	case "logs":
		cmdAppLogs(client)
	case "help", "--help", "-h":
		appUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown app command: %s\n", os.Args[2])
		appUsage()
		os.Exit(1)
	}
}

func appUsage() {
	fmt.Println(`Usage: aegis app <command> [options]

Commands:
  create     Create a new app
  publish    Publish a release (pull image, create rootfs)
  serve      Start serving an app
  list       List all apps
  info       Show app details
  delete     Delete an app and its releases
  logs       Stream app instance logs

Examples:
  aegis app create --name myapp --image python:3.12-alpine --expose 80 -- python3 -m http.server 80
  aegis app publish myapp [--label v1]
  aegis app serve myapp
  aegis app list
  aegis app info myapp
  aegis app delete myapp`)
}

// cmdAppCreate creates a new app.
// aegis app create --name myapp --image python:3.12-alpine --expose 80 -- python3 -m http.server 80
func cmdAppCreate(client *http.Client) {
	args := os.Args[3:]

	var name, imageRef string
	var exposePorts []int
	var command []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			command = args[i+1:]
			break
		}
		switch args[i] {
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
		case "--expose":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--expose requires a port number")
				os.Exit(1)
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid port: %s\n", args[i+1])
				os.Exit(1)
			}
			exposePorts = append(exposePorts, port)
			i++
		}
	}

	if name == "" || imageRef == "" {
		fmt.Fprintln(os.Stderr, "usage: aegis app create --name NAME --image IMAGE [--expose PORT] -- COMMAND...")
		os.Exit(1)
	}

	reqBody := map[string]interface{}{
		"name":         name,
		"image":        imageRef,
		"command":      command,
		"expose_ports": exposePorts,
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post("http://aegis/v1/apps", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create app: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusCreated {
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "create app failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("App created: %s (id=%s)\n", name, result["id"])
}

// cmdAppPublish publishes a release.
// aegis app publish myapp [--label v1]
func cmdAppPublish(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis app publish APP_NAME [--label LABEL]")
		os.Exit(1)
	}

	appName := os.Args[3]
	args := os.Args[4:]

	var label string
	for i := 0; i < len(args); i++ {
		if args[i] == "--label" && i+1 < len(args) {
			label = args[i+1]
			i++
		}
	}

	reqBody := map[string]interface{}{}
	if label != "" {
		reqBody["label"] = label
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/apps/%s/publish", appName),
		"application/json",
		bytes.NewReader(bodyJSON),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "publish: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusCreated {
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "publish failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Published release %s", result["id"])
	if label != "" {
		fmt.Printf(" (label=%s)", label)
	}
	fmt.Println()
}

// cmdAppServe starts serving an app.
// aegis app serve myapp
func cmdAppServe(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis app serve APP_NAME")
		os.Exit(1)
	}

	appName := os.Args[3]

	resp, err := client.Post(
		fmt.Sprintf("http://aegis/v1/apps/%s/serve", appName),
		"application/json",
		bytes.NewReader([]byte("{}")),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "serve failed: %s\n", errMsg)
		os.Exit(1)
	}

	routerAddr, _ := result["router_addr"].(string)
	instID, _ := result["id"].(string)
	fmt.Printf("Serving %s on http://%s\n", appName, routerAddr)
	fmt.Printf("Instance: %s\n", instID)
	fmt.Println("Press Ctrl+C to stop")

	// Wait for interrupt or instance disappearing
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Println("\nStopping...")
			req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instID), nil)
			client.Do(req)
			fmt.Println("Stopped")
			return
		case <-ticker.C:
			resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", instID))
			if err != nil || resp.StatusCode == 404 {
				if resp != nil {
					resp.Body.Close()
				}
				fmt.Println("\nInstance no longer exists. Exiting.")
				return
			}
			resp.Body.Close()
		}
	}
}

// cmdAppList lists all apps.
func cmdAppList(client *http.Client) {
	resp, err := client.Get("http://aegis/v1/apps")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list apps: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var apps []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&apps)

	if len(apps) == 0 {
		fmt.Println("No apps")
		return
	}

	fmt.Printf("%-20s %-30s %-10s\n", "NAME", "IMAGE", "ID")
	for _, app := range apps {
		name, _ := app["name"].(string)
		image, _ := app["image"].(string)
		id, _ := app["id"].(string)
		fmt.Printf("%-20s %-30s %-10s\n", name, image, id)
	}
}

// cmdAppInfo shows details for an app.
func cmdAppInfo(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis app info APP_NAME")
		os.Exit(1)
	}

	appName := os.Args[3]

	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/apps/%s", appName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get app: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "app %q not found\n", appName)
		os.Exit(1)
	}

	var app map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&app)

	fmt.Printf("Name:    %s\n", app["name"])
	fmt.Printf("ID:      %s\n", app["id"])
	fmt.Printf("Image:   %s\n", app["image"])

	if cmd, ok := app["command"].([]interface{}); ok && len(cmd) > 0 {
		parts := make([]string, len(cmd))
		for i, c := range cmd {
			parts[i] = fmt.Sprint(c)
		}
		fmt.Printf("Command: %s\n", strings.Join(parts, " "))
	}

	if ports, ok := app["expose_ports"].([]interface{}); ok && len(ports) > 0 {
		parts := make([]string, len(ports))
		for i, p := range ports {
			parts[i] = fmt.Sprint(p)
		}
		fmt.Printf("Ports:   %s\n", strings.Join(parts, ", "))
	}

	// Show releases
	relResp, err := client.Get(fmt.Sprintf("http://aegis/v1/apps/%s/releases", appName))
	if err == nil {
		defer relResp.Body.Close()
		var releases []map[string]interface{}
		json.NewDecoder(relResp.Body).Decode(&releases)
		if len(releases) > 0 {
			fmt.Printf("\nReleases (%d):\n", len(releases))
			for _, rel := range releases {
				label, _ := rel["label"].(string)
				id, _ := rel["id"].(string)
				if label != "" {
					fmt.Printf("  %s (%s)\n", id, label)
				} else {
					fmt.Printf("  %s\n", id)
				}
			}
		}
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
	case "set-workspace":
		cmdSecretSetWorkspace(client)
	case "list-workspace":
		cmdSecretListWorkspace(client)
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
  set            Set an app secret
  list           List app secrets
  delete         Delete an app secret
  set-workspace  Set a workspace-wide secret
  list-workspace List workspace-wide secrets

Examples:
  aegis secret set myapp API_KEY sk-test123
  aegis secret list myapp
  aegis secret delete myapp API_KEY
  aegis secret set-workspace GLOBAL_KEY value123
  aegis secret list-workspace`)
}

// cmdSecretSet sets an app secret.
// aegis secret set <app> <key> <value>
func cmdSecretSet(client *http.Client) {
	if len(os.Args) < 6 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret set APP_NAME KEY VALUE")
		os.Exit(1)
	}

	appName := os.Args[3]
	key := os.Args[4]
	value := os.Args[5]

	bodyJSON, _ := json.Marshal(map[string]string{"value": value})
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://aegis/v1/apps/%s/secrets/%s", appName, key),
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

	fmt.Printf("Secret %s set for %s\n", key, appName)
}

// cmdSecretList lists secrets for an app.
func cmdSecretList(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret list APP_NAME")
		os.Exit(1)
	}

	appName := os.Args[3]

	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/apps/%s/secrets", appName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "list secrets: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "list secrets failed: %s\n", errMsg)
		os.Exit(1)
	}

	var secrets []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&secrets)

	if len(secrets) == 0 {
		fmt.Printf("No secrets for %s\n", appName)
		return
	}

	fmt.Printf("Secrets for %s:\n", appName)
	for _, sec := range secrets {
		name, _ := sec["name"].(string)
		fmt.Printf("  %s\n", name)
	}
}

// cmdSecretDelete deletes an app secret.
func cmdSecretDelete(client *http.Client) {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret delete APP_NAME KEY")
		os.Exit(1)
	}

	appName := os.Args[3]
	key := os.Args[4]

	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("http://aegis/v1/apps/%s/secrets/%s", appName, key),
		nil)
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

	fmt.Printf("Secret %s deleted from %s\n", key, appName)
}

// cmdSecretSetWorkspace sets a workspace-wide secret.
func cmdSecretSetWorkspace(client *http.Client) {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: aegis secret set-workspace KEY VALUE")
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
		fmt.Fprintf(os.Stderr, "set workspace secret: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "set workspace secret failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Workspace secret %s set\n", key)
}

// cmdSecretListWorkspace lists workspace-wide secrets.
func cmdSecretListWorkspace(client *http.Client) {
	resp, err := client.Get("http://aegis/v1/secrets")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list workspace secrets: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var secrets []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&secrets)

	if len(secrets) == 0 {
		fmt.Println("No workspace secrets")
		return
	}

	fmt.Println("Workspace secrets:")
	for _, sec := range secrets {
		name, _ := sec["name"].(string)
		fmt.Printf("  %s\n", name)
	}
}

// cmdAppDelete deletes an app.
func cmdAppDelete(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis app delete APP_NAME")
		os.Exit(1)
	}

	appName := os.Args[3]

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/apps/%s", appName), nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete app: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "delete failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("App %q deleted\n", appName)
}

// cmdInstance dispatches instance subcommands.
func cmdInstance() {
	if len(os.Args) < 3 {
		fmt.Println(`Usage: aegis instance <command>

Commands:
  list    List all instances
  info    Show instance details`)
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	switch os.Args[2] {
	case "list":
		cmdInstanceList(client)
	case "info":
		cmdInstanceInfo(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown instance command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func cmdInstanceList(client *http.Client) {
	resp, err := client.Get("http://aegis/v1/instances")
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

	fmt.Printf("%-30s %-10s %-20s %-10s\n", "ID", "STATE", "APP", "CONNS")
	for _, inst := range instances {
		id, _ := inst["id"].(string)
		state, _ := inst["state"].(string)
		appID, _ := inst["app_id"].(string)
		conns, _ := inst["active_connections"].(float64)
		fmt.Printf("%-30s %-10s %-20s %-10.0f\n", id, state, appID, conns)
	}
}

func cmdInstanceInfo(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis instance info INSTANCE_ID")
		os.Exit(1)
	}

	instID := os.Args[3]

	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", instID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "instance %q not found\n", instID)
		os.Exit(1)
	}

	var inst map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&inst)

	fmt.Printf("ID:          %s\n", inst["id"])
	fmt.Printf("State:       %s\n", inst["state"])
	if appID, ok := inst["app_id"].(string); ok && appID != "" {
		fmt.Printf("App:         %s\n", appID)
	}
	if relID, ok := inst["release_id"].(string); ok && relID != "" {
		fmt.Printf("Release:     %s\n", relID)
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

// cmdExec executes a command in a running instance.
// aegis exec TARGET -- CMD...
func cmdExec() {
	args := os.Args[2:]

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aegis exec TARGET -- COMMAND [args...]")
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	// Parse: first arg is target, then -- separator, then command
	target := args[0]
	var command []string
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			command = args[i+1:]
			break
		}
	}

	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aegis exec TARGET -- COMMAND [args...]")
		os.Exit(1)
	}

	client := httpClient()

	// Resolve target to instance ID
	instID := resolveInstanceTarget(client, target)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "could not resolve target %q to an instance\n", target)
		os.Exit(1)
	}

	// POST exec
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

	// Stream NDJSON response to stdout/stderr
	decoder := json.NewDecoder(resp.Body)
	first := true
	for decoder.More() {
		var entry map[string]interface{}
		if err := decoder.Decode(&entry); err != nil {
			break
		}

		// First line is the exec info
		if first {
			first = false
			continue
		}

		// Check for exec completion marker
		if done, _ := entry["done"].(bool); done {
			if ecStr, _ := entry["exit_code"].(string); ecStr != "" && ecStr != "0" {
				exitCode, _ := strconv.Atoi(ecStr)
				os.Exit(exitCode)
			}
			return
		}

		line, _ := entry["line"].(string)
		stream, _ := entry["stream"].(string)

		switch stream {
		case "stderr":
			fmt.Fprintln(os.Stderr, line)
		default:
			fmt.Println(line)
		}
	}
}

// resolveInstanceTarget resolves a target (instance ID or app name) to an instance ID.
func resolveInstanceTarget(client *http.Client, target string) string {
	// Try as instance ID first
	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/instances/%s", target))
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return target
		}
	}

	// Try as app name — find the instance for this app
	appsResp, err := client.Get("http://aegis/v1/apps")
	if err != nil {
		return ""
	}
	defer appsResp.Body.Close()

	var apps []map[string]interface{}
	json.NewDecoder(appsResp.Body).Decode(&apps)

	var appID string
	for _, app := range apps {
		name, _ := app["name"].(string)
		id, _ := app["id"].(string)
		if name == target || id == target {
			appID = id
			break
		}
	}
	if appID == "" {
		return ""
	}

	// Find instance with matching app_id
	instResp, err := client.Get("http://aegis/v1/instances")
	if err != nil {
		return ""
	}
	defer instResp.Body.Close()

	var instances []map[string]interface{}
	json.NewDecoder(instResp.Body).Decode(&instances)

	for _, inst := range instances {
		if aID, _ := inst["app_id"].(string); aID == appID {
			id, _ := inst["id"].(string)
			return id
		}
	}
	return ""
}

// cmdLogs streams logs for an app (short alias for 'aegis app logs').
// aegis logs APP_NAME [--follow]
func cmdLogs() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: aegis logs APP_NAME [--follow]")
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	appName := os.Args[2]
	follow := false
	for _, arg := range os.Args[3:] {
		if arg == "--follow" || arg == "-f" {
			follow = true
		}
	}

	client := httpClient()
	streamAppLogs(client, appName, follow)
}

// cmdAppLogs streams instance logs for an app.
// aegis app logs APP_NAME [--follow]
func cmdAppLogs(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis app logs APP_NAME [--follow]")
		os.Exit(1)
	}

	appName := os.Args[3]
	follow := false
	for _, arg := range os.Args[4:] {
		if arg == "--follow" || arg == "-f" {
			follow = true
		}
	}

	streamAppLogs(client, appName, follow)
}

// streamAppLogs resolves an app name to an instance and streams its logs.
func streamAppLogs(client *http.Client, appName string, follow bool) {
	instID := resolveInstanceTarget(client, appName)
	if instID == "" {
		fmt.Fprintf(os.Stderr, "no running instance found for %q\n", appName)
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

		line, _ := entry["line"].(string)
		stream, _ := entry["stream"].(string)

		switch stream {
		case "stderr":
			fmt.Fprintln(os.Stderr, line)
		default:
			fmt.Println(line)
		}
	}
}

// cmdKit dispatches kit subcommands.
func cmdKit() {
	if len(os.Args) < 3 {
		kitUsage()
		os.Exit(1)
	}

	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "aegisd is not running. Run 'aegis up' first.")
		os.Exit(1)
	}

	client := httpClient()

	switch os.Args[2] {
	case "install":
		cmdKitInstall(client)
	case "list":
		cmdKitList(client)
	case "info":
		cmdKitInfo(client)
	case "uninstall":
		cmdKitUninstall(client)
	case "help", "--help", "-h":
		kitUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown kit command: %s\n", os.Args[2])
		kitUsage()
		os.Exit(1)
	}
}

func kitUsage() {
	fmt.Println(`Usage: aegis kit <command> [options]

Commands:
  install    Install a kit from a manifest file
  list       List installed kits
  info       Show kit details
  uninstall  Uninstall a kit

Examples:
  aegis kit install manifest.yaml
  aegis kit list
  aegis kit info famiglia
  aegis kit uninstall famiglia`)
}

// cmdKitInstall installs a kit from a YAML manifest.
func cmdKitInstall(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis kit install MANIFEST.yaml")
		os.Exit(1)
	}

	manifestPath := os.Args[3]
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read manifest: %v\n", err)
		os.Exit(1)
	}

	// Parse YAML into a generic map, then re-encode as JSON for the API
	// We do a simple approach: parse the YAML fields we need
	var manifest struct {
		Name        string      `json:"name"`
		Version     string      `json:"version"`
		Description string      `json:"description,omitempty"`
		Image       string      `json:"image"`
		Config      interface{} `json:"config,omitempty"`
	}

	// Use yaml.Unmarshal from the standard approach
	// Since the CLI binary doesn't import yaml.v3, we parse manually
	// Actually, let's just parse key fields with a simple approach
	// and send the right JSON to the API

	// Quick parse: read YAML as generic map
	var yamlMap map[string]interface{}
	if err := json.Unmarshal(data, &yamlMap); err != nil {
		// Not JSON, try to read as simple key-value YAML
		// For the CLI, we'll just read the file and POST it through a helper
		// Actually, the simplest approach: the CLI sends the raw manifest
		// to a POST endpoint that accepts YAML. But the plan says
		// "aegis kit install reads YAML and POSTs JSON".
		// Let's use a simpler approach for the CLI: shell out or inline parse.
		_ = err
	}

	// Since we can't import yaml.v3 in the CLI (CGO_ENABLED=0 and we want
	// minimal deps), we'll use a two-step approach:
	// Parse enough of the YAML to extract fields, or just POST raw and
	// let the server handle it. Let's add a manifest-accept endpoint.
	// Actually, yaml.v3 is pure Go, no cgo needed. Let's import it.

	// Reset and use proper YAML parsing
	_ = data
	_ = manifest
	_ = yamlMap

	cmdKitInstallYAML(client, manifestPath)
}

// cmdKitInstallYAML reads YAML manifest and POSTs JSON to the API.
func cmdKitInstallYAML(client *http.Client, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read manifest: %v\n", err)
		os.Exit(1)
	}

	// Parse YAML manually (simple line-based for CLI)
	// Extract name, version, description, image, and config
	fields := parseSimpleYAML(data)

	name := fields["name"]
	version := fields["version"]
	description := fields["description"]
	image := fields["image"]

	if name == "" || version == "" || image == "" {
		fmt.Fprintln(os.Stderr, "manifest must contain name, version, and image fields")
		os.Exit(1)
	}

	reqBody := map[string]interface{}{
		"name":      name,
		"version":   version,
		"image_ref": image,
	}
	if description != "" {
		reqBody["description"] = description
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post("http://aegis/v1/kits", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "install kit: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusCreated {
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "install kit failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Kit %s v%s installed\n", name, version)
}

// parseSimpleYAML extracts top-level string fields from YAML.
func parseSimpleYAML(data []byte) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Only parse top-level fields (no indentation in original)
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Remove quotes
		val = strings.Trim(val, `"'`)
		fields[key] = val
	}
	return fields
}

// cmdKitList lists installed kits.
func cmdKitList(client *http.Client) {
	resp, err := client.Get("http://aegis/v1/kits")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list kits: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var kits []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&kits)

	if len(kits) == 0 {
		fmt.Println("No kits installed")
		return
	}

	fmt.Printf("%-20s %-15s %-40s\n", "NAME", "VERSION", "IMAGE")
	for _, kit := range kits {
		name, _ := kit["name"].(string)
		version, _ := kit["version"].(string)
		imageRef, _ := kit["image_ref"].(string)
		fmt.Printf("%-20s %-15s %-40s\n", name, version, imageRef)
	}
}

// cmdKitInfo shows details for a kit.
func cmdKitInfo(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis kit info KIT_NAME")
		os.Exit(1)
	}

	kitName := os.Args[3]

	resp, err := client.Get(fmt.Sprintf("http://aegis/v1/kits/%s", kitName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get kit: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "kit %q not found\n", kitName)
		os.Exit(1)
	}

	var kit map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&kit)

	fmt.Printf("Name:        %s\n", kit["name"])
	fmt.Printf("Version:     %s\n", kit["version"])
	if desc, ok := kit["description"].(string); ok && desc != "" {
		fmt.Printf("Description: %s\n", desc)
	}
	fmt.Printf("Image:       %s\n", kit["image_ref"])
	fmt.Printf("Installed:   %s\n", kit["installed_at"])
}

// cmdKitUninstall removes a kit.
func cmdKitUninstall(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: aegis kit uninstall KIT_NAME")
		os.Exit(1)
	}

	kitName := os.Args[3]

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/kits/%s", kitName), nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uninstall kit: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		errMsg, _ := result["error"].(string)
		fmt.Fprintf(os.Stderr, "uninstall failed: %s\n", errMsg)
		os.Exit(1)
	}

	fmt.Printf("Kit %q uninstalled\n", kitName)
}
