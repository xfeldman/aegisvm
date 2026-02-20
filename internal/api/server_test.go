package api

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/secrets"
)

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	reg, err := registry.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	ss, err := secrets.NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		registry:    reg,
		secretStore: ss,
	}
	return s
}

func seedSecret(t *testing.T, s *Server, name, value string) {
	t.Helper()
	encrypted, err := s.secretStore.EncryptString(value)
	if err != nil {
		t.Fatal(err)
	}
	err = s.registry.SaveSecret(&registry.Secret{
		ID:             "sec-" + name,
		Name:           name,
		EncryptedValue: encrypted,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveEnv_NoSecrets(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "sk-123")
	seedSecret(t, s, "DB_URL", "postgres://...")

	// Default: no secrets requested → empty env
	env := s.resolveEnv(nil, nil)
	if len(env) != 0 {
		t.Fatalf("expected empty env, got %v", env)
	}

	env = s.resolveEnv([]string{}, nil)
	if len(env) != 0 {
		t.Fatalf("expected empty env for [], got %v", env)
	}
}

func TestResolveEnv_AllSecrets(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "sk-123")
	seedSecret(t, s, "DB_URL", "postgres://...")

	env := s.resolveEnv([]string{"*"}, nil)
	if len(env) != 2 {
		t.Fatalf("expected 2 secrets, got %d: %v", len(env), env)
	}
	if env["API_KEY"] != "sk-123" {
		t.Errorf("API_KEY = %q, want %q", env["API_KEY"], "sk-123")
	}
	if env["DB_URL"] != "postgres://..." {
		t.Errorf("DB_URL = %q, want %q", env["DB_URL"], "postgres://...")
	}
}

func TestResolveEnv_Allowlist(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "sk-123")
	seedSecret(t, s, "DB_URL", "postgres://...")
	seedSecret(t, s, "OTHER", "not-wanted")

	env := s.resolveEnv([]string{"API_KEY", "DB_URL"}, nil)
	if len(env) != 2 {
		t.Fatalf("expected 2 secrets, got %d: %v", len(env), env)
	}
	if env["API_KEY"] != "sk-123" {
		t.Errorf("API_KEY = %q, want %q", env["API_KEY"], "sk-123")
	}
	if env["DB_URL"] != "postgres://..." {
		t.Errorf("DB_URL = %q, want %q", env["DB_URL"], "postgres://...")
	}
	if _, ok := env["OTHER"]; ok {
		t.Error("OTHER should not be injected")
	}
}

func TestResolveEnv_AllowlistMissing(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "sk-123")

	// Request a key that doesn't exist in the store
	env := s.resolveEnv([]string{"API_KEY", "NONEXISTENT"}, nil)
	if len(env) != 1 {
		t.Fatalf("expected 1 secret, got %d: %v", len(env), env)
	}
	if env["API_KEY"] != "sk-123" {
		t.Errorf("API_KEY = %q, want %q", env["API_KEY"], "sk-123")
	}
}

func TestResolveEnv_ExplicitEnvOverridesSecret(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "from-store")

	explicit := map[string]string{"API_KEY": "from-env"}
	env := s.resolveEnv([]string{"API_KEY"}, explicit)

	if env["API_KEY"] != "from-env" {
		t.Errorf("API_KEY = %q, want %q (explicit env should override secret)", env["API_KEY"], "from-env")
	}
}

func TestResolveEnv_ExplicitEnvWithoutSecrets(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "should-not-appear")

	explicit := map[string]string{"CUSTOM": "value"}
	env := s.resolveEnv(nil, explicit)

	if len(env) != 1 {
		t.Fatalf("expected 1 env var, got %d: %v", len(env), env)
	}
	if env["CUSTOM"] != "value" {
		t.Errorf("CUSTOM = %q, want %q", env["CUSTOM"], "value")
	}
	if _, ok := env["API_KEY"]; ok {
		t.Error("API_KEY should not be injected when no secrets requested")
	}
}

func TestResolveEnv_ExplicitEnvMergedWithAllSecrets(t *testing.T) {
	s := setupTestServer(t)
	seedSecret(t, s, "API_KEY", "from-store")

	explicit := map[string]string{"CUSTOM": "value"}
	env := s.resolveEnv([]string{"*"}, explicit)

	if len(env) != 2 {
		t.Fatalf("expected 2 env vars, got %d: %v", len(env), env)
	}
	if env["API_KEY"] != "from-store" {
		t.Errorf("API_KEY = %q, want %q", env["API_KEY"], "from-store")
	}
	if env["CUSTOM"] != "value" {
		t.Errorf("CUSTOM = %q, want %q", env["CUSTOM"], "value")
	}
}

func TestResolveEnv_EmptyStore(t *testing.T) {
	s := setupTestServer(t)

	// Request all from empty store
	env := s.resolveEnv([]string{"*"}, nil)
	if len(env) != 0 {
		t.Fatalf("expected empty env from empty store, got %v", env)
	}

	// Request specific from empty store
	env = s.resolveEnv([]string{"API_KEY"}, nil)
	if len(env) != 0 {
		t.Fatalf("expected empty env for missing key, got %v", env)
	}
}

// --- resolveWorkspace tests ---

func TestResolveWorkspace_NamedWorkspace(t *testing.T) {
	s := &Server{
		cfg: &config.Config{WorkspacesDir: "/home/user/.aegis/data/workspaces"},
	}

	got := s.resolveWorkspace("claw")
	want := "/home/user/.aegis/data/workspaces/claw"
	if got != want {
		t.Errorf("resolveWorkspace(%q) = %q, want %q", "claw", got, want)
	}
}

func TestResolveWorkspace_NamedWorkspace_Hyphenated(t *testing.T) {
	s := &Server{
		cfg: &config.Config{WorkspacesDir: "/data/workspaces"},
	}

	got := s.resolveWorkspace("my-agent")
	want := "/data/workspaces/my-agent"
	if got != want {
		t.Errorf("resolveWorkspace(%q) = %q, want %q", "my-agent", got, want)
	}
}

func TestResolveWorkspace_PathWithSlash(t *testing.T) {
	s := &Server{
		cfg: &config.Config{WorkspacesDir: "/data/workspaces"},
	}

	got := s.resolveWorkspace("/absolute/path/to/project")
	if got != "/absolute/path/to/project" {
		t.Errorf("resolveWorkspace with absolute path = %q, want %q", got, "/absolute/path/to/project")
	}
}

func TestResolveWorkspace_PathWithDot(t *testing.T) {
	s := &Server{
		cfg: &config.Config{WorkspacesDir: "/data/workspaces"},
	}

	got := s.resolveWorkspace("./myapp")
	// Should resolve to absolute path, not named workspace
	if got == "/data/workspaces/./myapp" {
		t.Errorf("resolveWorkspace(%q) should not treat dot-path as named workspace", "./myapp")
	}
}

func TestResolveWorkspace_RelativeWithSlash(t *testing.T) {
	s := &Server{
		cfg: &config.Config{WorkspacesDir: "/data/workspaces"},
	}

	got := s.resolveWorkspace("foo/bar")
	// Contains slash → treated as path, not named
	if got == "/data/workspaces/foo/bar" {
		t.Errorf("resolveWorkspace(%q) should not treat path-with-slash as named workspace", "foo/bar")
	}
}

// --- parseDuration tests ---

func TestParseDuration_Days(t *testing.T) {
	d, err := parseDuration("7d")
	if err != nil {
		t.Fatal(err)
	}
	if d != 7*24*time.Hour {
		t.Errorf("parseDuration(%q) = %v, want %v", "7d", d, 7*24*time.Hour)
	}
}

func TestParseDuration_SingleDay(t *testing.T) {
	d, err := parseDuration("1d")
	if err != nil {
		t.Fatal(err)
	}
	if d != 24*time.Hour {
		t.Errorf("parseDuration(%q) = %v, want %v", "1d", d, 24*time.Hour)
	}
}

func TestParseDuration_Hours(t *testing.T) {
	d, err := parseDuration("24h")
	if err != nil {
		t.Fatal(err)
	}
	if d != 24*time.Hour {
		t.Errorf("parseDuration(%q) = %v, want %v", "24h", d, 24*time.Hour)
	}
}

func TestParseDuration_Minutes(t *testing.T) {
	d, err := parseDuration("30m")
	if err != nil {
		t.Fatal(err)
	}
	if d != 30*time.Minute {
		t.Errorf("parseDuration(%q) = %v, want %v", "30m", d, 30*time.Minute)
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	_, err := parseDuration("abc")
	if err == nil {
		t.Error("expected error for invalid duration")
	}
}
