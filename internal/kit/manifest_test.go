package kit

import (
	"testing"
)

const testManifestYAML = `
name: famiglia
version: "0.1.0"
description: "Famiglia AI agent kit"
image: ghcr.io/famiglia/agent:latest

config:
  secrets:
    required:
      - name: ANTHROPIC_API_KEY
        description: "Anthropic API key for Claude"
    optional:
      - name: OPENAI_API_KEY
        description: "OpenAI API key (fallback)"
  routing:
    default_port: 8080
    healthcheck: /health
    headers:
      X-Kit-Name: famiglia
  networking:
    egress:
      - "api.anthropic.com"
      - "api.openai.com"
  policies:
    max_memory_mb: 4096
    max_vcpus: 4
  resources:
    memory_mb: 1024
    vcpus: 2
`

func TestParseBytes(t *testing.T) {
	m, err := ParseBytes([]byte(testManifestYAML))
	if err != nil {
		t.Fatal(err)
	}

	if m.Name != "famiglia" {
		t.Fatalf("name: got %q", m.Name)
	}
	if m.Version != "0.1.0" {
		t.Fatalf("version: got %q", m.Version)
	}
	if m.Image != "ghcr.io/famiglia/agent:latest" {
		t.Fatalf("image: got %q", m.Image)
	}
	if len(m.Config.Secrets.Required) != 1 {
		t.Fatalf("required secrets: got %d", len(m.Config.Secrets.Required))
	}
	if m.Config.Secrets.Required[0].Name != "ANTHROPIC_API_KEY" {
		t.Fatalf("required secret name: got %q", m.Config.Secrets.Required[0].Name)
	}
	if m.Config.Routing.DefaultPort != 8080 {
		t.Fatalf("default_port: got %d", m.Config.Routing.DefaultPort)
	}
	if m.Config.Routing.Healthcheck != "/health" {
		t.Fatalf("healthcheck: got %q", m.Config.Routing.Healthcheck)
	}
	if len(m.Config.Networking.Egress) != 2 {
		t.Fatalf("egress: got %d", len(m.Config.Networking.Egress))
	}
	if m.Config.Resources.MemoryMB != 1024 {
		t.Fatalf("memory_mb: got %d", m.Config.Resources.MemoryMB)
	}
}

func TestToKit(t *testing.T) {
	m, err := ParseBytes([]byte(testManifestYAML))
	if err != nil {
		t.Fatal(err)
	}

	k := m.ToKit()
	if k.Name != "famiglia" {
		t.Fatalf("name: got %q", k.Name)
	}
	if k.ImageRef != "ghcr.io/famiglia/agent:latest" {
		t.Fatalf("image_ref: got %q", k.ImageRef)
	}
	if len(k.Config.Secrets.Required) != 1 {
		t.Fatalf("required secrets: got %d", len(k.Config.Secrets.Required))
	}
	if k.Config.Routing.DefaultPort != 8080 {
		t.Fatalf("routing port: got %d", k.Config.Routing.DefaultPort)
	}
	if k.Config.Resources.MemoryMB != 1024 {
		t.Fatalf("memory: got %d", k.Config.Resources.MemoryMB)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing name", `version: "1.0"\nimage: foo`},
		{"missing version", `name: test\nimage: foo`},
		{"missing image", `name: test\nversion: "1.0"`},
		{"invalid yaml", `{{{`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestMinimalManifest(t *testing.T) {
	yaml := `
name: minimal
version: "1.0.0"
image: alpine:latest
`
	m, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "minimal" {
		t.Fatalf("name: got %q", m.Name)
	}
	k := m.ToKit()
	if k.Name != "minimal" {
		t.Fatalf("kit name: got %q", k.Name)
	}
}
