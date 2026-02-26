// aegis-claw-bootstrap is the bootstrap binary for the OpenClaw tether kit.
//
// It runs inside the VM as the initial process, handles first-boot setup
// (npm install, config generation), then exec's into the OpenClaw gateway.
// After exec, this process is gone — OpenClaw is the main process.
//
// Config files are generated from embedded templates + environment variables.
// Structural config is write-if-missing (user can customize). Auth profiles
// are always rewritten (secrets may rotate between restarts).
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o aegis-claw-bootstrap ./cmd/aegis-claw-bootstrap
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"
)

const (
	workspaceRoot  = "/workspace"
	openclawHome   = "/workspace/.openclaw"
	configDir      = "/workspace/.openclaw/.openclaw"
	npmPrefix      = "/workspace/.npm-global"
	openclawBin    = "/workspace/.npm-global/bin/openclaw"
	nodeHeapSize   = "384"
	openclawVersion = "2026.2.24"
)

//go:embed templates/openclaw.json.tmpl
var openclawConfigTmpl string

//go:embed templates/agents.md
var agentsMdDefault string

//go:embed templates/mcp.json
var mcpConfigDefault string

//go:embed channel-extension.tgz
var channelExtensionTgz []byte

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("aegis-claw-bootstrap starting")

	// 1. Set environment
	setEnv()

	// 2. First-boot install
	if err := ensureInstalled(); err != nil {
		log.Fatalf("install failed: %v", err)
	}

	// Debug: log API key availability
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		log.Println("ANTHROPIC_API_KEY: present")
	} else {
		log.Println("ANTHROPIC_API_KEY: not set")
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		log.Println("OPENAI_API_KEY: present")
	} else {
		log.Println("OPENAI_API_KEY: not set")
	}

	// 3. Generate config
	if err := generateConfig(); err != nil {
		log.Fatalf("config generation failed: %v", err)
	}

	// 4. Exec into OpenClaw gateway
	execOpenClaw()
}

func setEnv() {
	os.Setenv("HOME", workspaceRoot)
	os.Setenv("OPENCLAW_HOME", openclawHome)
	os.Setenv("NODE_OPTIONS", fmt.Sprintf("--max-old-space-size=%s", nodeHeapSize))
	os.Setenv("npm_config_prefix", npmPrefix)

	// Prepend npm bin to PATH
	path := os.Getenv("PATH")
	os.Setenv("PATH", filepath.Join(npmPrefix, "bin")+":"+path)
}

func ensureInstalled() error {
	if _, err := os.Stat(openclawBin); err == nil {
		log.Println("OpenClaw already installed, skipping npm install")
		return nil
	}

	log.Printf("first boot: installing openclaw@%s + channel extension", openclawVersion)

	// Extract embedded channel extension tarball to temp file
	tmpTgz, err := os.CreateTemp("", "openclaw-channel-aegis-*.tgz")
	if err != nil {
		return fmt.Errorf("create temp tgz: %w", err)
	}
	defer os.Remove(tmpTgz.Name())
	if _, err := tmpTgz.Write(channelExtensionTgz); err != nil {
		tmpTgz.Close()
		return fmt.Errorf("write temp tgz: %w", err)
	}
	tmpTgz.Close()

	// npm install both OpenClaw and the channel extension
	cmd := exec.Command("npm", "install", "-g",
		fmt.Sprintf("openclaw@%s", openclawVersion),
		tmpTgz.Name(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install: %w", err)
	}

	log.Println("npm install complete")
	return nil
}

func generateConfig() error {
	// Ensure config directories exist
	os.MkdirAll(configDir, 0755)
	os.MkdirAll(filepath.Join(configDir, "credentials"), 0755)
	os.MkdirAll(filepath.Join(workspaceRoot, "canvas"), 0755)

	// Detect model from env
	model, embeddingProvider := detectModel()

	// File 1: openclaw.json (write if missing)
	openclawConfigPath := filepath.Join(configDir, "openclaw.json")
	if err := writeIfMissing(openclawConfigPath, func() ([]byte, error) {
		return renderTemplate(openclawConfigTmpl, map[string]string{
			"Model":             model,
			"EmbeddingProvider": embeddingProvider,
		})
	}); err != nil {
		return fmt.Errorf("openclaw.json: %w", err)
	}

	// File 2: auth-profiles.json (always rewrite)
	authPath := filepath.Join(configDir, "credentials", "auth-profiles.json")
	if err := writeAuthProfiles(authPath); err != nil {
		return fmt.Errorf("auth-profiles.json: %w", err)
	}

	// File 3: AGENTS.md (write if missing)
	agentsPath := filepath.Join(configDir, "AGENTS.md")
	if err := writeIfMissing(agentsPath, func() ([]byte, error) {
		return []byte(agentsMdDefault), nil
	}); err != nil {
		return fmt.Errorf("AGENTS.md: %w", err)
	}

	// File 4: mcp.json (write if missing)
	mcpPath := filepath.Join(configDir, "mcp.json")
	if err := writeIfMissing(mcpPath, func() ([]byte, error) {
		return []byte(mcpConfigDefault), nil
	}); err != nil {
		return fmt.Errorf("mcp.json: %w", err)
	}

	log.Println("config generation complete")
	return nil
}

func detectModel() (model, embeddingProvider string) {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic/claude-sonnet-4-20250514", "anthropic"
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "openai/gpt-4o", "openai"
	}
	// Fallback — shouldn't happen if required_secrets is enforced
	return "openai/gpt-4o", "openai"
}

func renderTemplate(tmplStr string, data map[string]string) ([]byte, error) {
	tmpl, err := template.New("config").Parse(tmplStr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeIfMissing(path string, gen func() ([]byte, error)) error {
	if _, err := os.Stat(path); err == nil {
		log.Printf("  %s exists, skipping", filepath.Base(path))
		return nil
	}
	data, err := gen()
	if err != nil {
		return err
	}
	log.Printf("  writing %s", filepath.Base(path))
	return os.WriteFile(path, data, 0644)
}

func writeAuthProfiles(path string) error {
	profiles := map[string]interface{}{}

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		profiles["anthropic:default"] = map[string]string{
			"type": "api_key", "provider": "anthropic", "key": key,
		}
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		profiles["openai:default"] = map[string]string{
			"type": "api_key", "provider": "openai", "key": key,
		}
	}

	data, err := json.MarshalIndent(map[string]interface{}{
		"version":  1,
		"profiles": profiles,
	}, "", "  ")
	if err != nil {
		return err
	}

	log.Printf("  writing auth-profiles.json (%d profiles)", len(profiles))
	return os.WriteFile(path, data, 0644)
}

func execOpenClaw() {
	binary, err := exec.LookPath("openclaw")
	if err != nil {
		log.Fatalf("openclaw not found in PATH: %v", err)
	}

	args := []string{"openclaw", "gateway", "--allow-unconfigured"}
	log.Printf("exec: %s %v", binary, args[1:])

	// Replace this process with OpenClaw
	if err := syscall.Exec(binary, args, os.Environ()); err != nil {
		log.Fatalf("exec failed: %v", err)
	}
}
