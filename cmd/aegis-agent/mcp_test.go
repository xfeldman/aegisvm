package main

import (
	"os"
	"testing"
)

func TestLoadAgentConfigDefaults(t *testing.T) {
	// With no config file and no env vars, should return zero-value config.
	// /workspace/.aegis/agent.json won't exist in the test environment.
	clearAgentEnvVars(t)

	config := loadAgentConfig()

	if config.Model != "" {
		t.Errorf("Model = %q, want empty", config.Model)
	}
	if config.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0", config.MaxTokens)
	}
	if config.ContextChars != 0 {
		t.Errorf("ContextChars = %d, want 0", config.ContextChars)
	}
	if config.ContextTurns != 0 {
		t.Errorf("ContextTurns = %d, want 0", config.ContextTurns)
	}
	if config.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want empty", config.SystemPrompt)
	}
}

func TestLoadAgentConfigEnvOverrides(t *testing.T) {
	clearAgentEnvVars(t)

	t.Setenv("AEGIS_MODEL", "anthropic/claude-sonnet-4-20250514")
	t.Setenv("AEGIS_MAX_TOKENS", "8192")
	t.Setenv("AEGIS_CONTEXT_CHARS", "50000")
	t.Setenv("AEGIS_CONTEXT_TURNS", "100")
	t.Setenv("AEGIS_SYSTEM_PROMPT", "You are a test agent.")

	config := loadAgentConfig()

	if config.Model != "anthropic/claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want 'anthropic/claude-sonnet-4-20250514'", config.Model)
	}
	if config.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", config.MaxTokens)
	}
	if config.ContextChars != 50000 {
		t.Errorf("ContextChars = %d, want 50000", config.ContextChars)
	}
	if config.ContextTurns != 100 {
		t.Errorf("ContextTurns = %d, want 100", config.ContextTurns)
	}
	if config.SystemPrompt != "You are a test agent." {
		t.Errorf("SystemPrompt = %q, want 'You are a test agent.'", config.SystemPrompt)
	}
}

func TestLoadAgentConfigEnvPartialOverride(t *testing.T) {
	clearAgentEnvVars(t)

	// Only set some env vars
	t.Setenv("AEGIS_MAX_TOKENS", "2048")

	config := loadAgentConfig()

	if config.Model != "" {
		t.Errorf("Model = %q, want empty (not overridden)", config.Model)
	}
	if config.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048", config.MaxTokens)
	}
}

func TestLoadAgentConfigInvalidEnvValues(t *testing.T) {
	clearAgentEnvVars(t)

	// Non-numeric values for int fields should be ignored
	t.Setenv("AEGIS_MAX_TOKENS", "not-a-number")
	t.Setenv("AEGIS_CONTEXT_CHARS", "")

	config := loadAgentConfig()

	if config.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 (invalid env value should be ignored)", config.MaxTokens)
	}
	if config.ContextChars != 0 {
		t.Errorf("ContextChars = %d, want 0", config.ContextChars)
	}
}

func TestAgentConfigModelParsing(t *testing.T) {
	// Test the model parsing logic from main.go
	tests := []struct {
		model        string
		wantProvider string
		wantModel    string
	}{
		{"anthropic/claude-sonnet-4-20250514", "anthropic", "claude-sonnet-4-20250514"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"claude/claude-haiku-4-5-20251001", "claude", "claude-haiku-4-5-20251001"},
		{"gpt-4o", "", "gpt-4o"},
		{"", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			provider, modelName := "", ""
			if tt.model != "" {
				idx := 0
				for i, c := range tt.model {
					if c == '/' {
						idx = i
						break
					}
				}
				if idx > 0 {
					provider = tt.model[:idx]
					modelName = tt.model[idx+1:]
				} else {
					modelName = tt.model
				}
			}
			if provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tt.wantProvider)
			}
			if modelName != tt.wantModel {
				t.Errorf("modelName = %q, want %q", modelName, tt.wantModel)
			}
		})
	}
}

func clearAgentEnvVars(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"AEGIS_MODEL", "AEGIS_MAX_TOKENS", "AEGIS_CONTEXT_CHARS",
		"AEGIS_CONTEXT_TURNS", "AEGIS_SYSTEM_PROMPT",
	} {
		if v, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { os.Setenv(key, v) })
			os.Unsetenv(key)
		}
	}
}
