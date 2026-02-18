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
  app        Manage apps (create, publish, serve, list, info, delete)

Examples:
  aegis up
  aegis run -- echo "hello from aegis"
  aegis run --image alpine:3.21 -- echo hello
  aegis run --expose 80 -- python -m http.server 80
  aegis app create --name myapp --image python:3.12 --expose 80 -- python -m http.server 80
  aegis app publish myapp
  aegis app serve myapp
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

	// Wait for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	fmt.Println("\nStopping instance...")

	// Delete instance
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instanceID), nil)
	client.Do(req)

	fmt.Println("Stopped")
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

	// Check daemon
	fmt.Println()
	if isDaemonRunning() {
		fmt.Printf("aegisd:   running\n")
	} else {
		fmt.Printf("aegisd:   not running\n")
	}
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

Examples:
  aegis app create --name myapp --image python:3.12 --expose 80 -- python -m http.server 80
  aegis app publish myapp [--label v1]
  aegis app serve myapp
  aegis app list
  aegis app info myapp
  aegis app delete myapp`)
}

// cmdAppCreate creates a new app.
// aegis app create --name myapp --image python:3.12 --expose 80 -- python -m http.server 80
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
	fmt.Printf("Serving %s on http://%s\n", appName, routerAddr)
	fmt.Printf("Instance: %s\n", result["id"])
	fmt.Println("Press Ctrl+C to stop")

	// Wait for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	fmt.Println("\nStopping...")
	instID, _ := result["id"].(string)
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://aegis/v1/instances/%s", instID), nil)
	client.Do(req)
	fmt.Println("Stopped")
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
