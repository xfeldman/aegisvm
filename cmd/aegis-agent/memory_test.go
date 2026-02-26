package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryStoreAndLoad(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})

	id1, err := ms.Store("User prefers Python", []string{"preference"}, "user")
	if err != nil {
		t.Fatalf("Store 1: %v", err)
	}
	id2, err := ms.Store("DB is Postgres on port 5432", []string{"infra"}, "workspace")
	if err != nil {
		t.Fatalf("Store 2: %v", err)
	}
	id3, err := ms.Store("Deploy to AWS us-east-1", nil, "")
	if err != nil {
		t.Fatalf("Store 3: %v", err)
	}

	if id1 != "m-0" || id2 != "m-1" || id3 != "m-2" {
		t.Errorf("IDs = %s, %s, %s — want m-0, m-1, m-2", id1, id2, id3)
	}

	// Load into a new store from same dir
	ms2 := NewMemoryStore(dir, MemoryConfig{})
	if len(ms2.entries) != 3 {
		t.Fatalf("loaded %d entries, want 3", len(ms2.entries))
	}
	if ms2.entries[0].Text != "User prefers Python" {
		t.Errorf("entry 0 text = %q", ms2.entries[0].Text)
	}
	if ms2.entries[2].Scope != "workspace" {
		t.Errorf("entry 2 scope = %q, want 'workspace' (default)", ms2.entries[2].Scope)
	}
	if ms2.nextID != 3 {
		t.Errorf("nextID = %d, want 3", ms2.nextID)
	}
}

func TestMemorySearch(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})
	ms.Store("User prefers Python over JavaScript", []string{"preference"}, "user")
	ms.Store("DB is Postgres on port 5432", []string{"infra"}, "workspace")
	ms.Store("Deploy to AWS us-east-1", []string{"infra", "deploy"}, "workspace")

	t.Run("query match", func(t *testing.T) {
		results := ms.Search("python", "")
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		if results[0].ID != "m-0" {
			t.Errorf("ID = %s, want m-0", results[0].ID)
		}
	})

	t.Run("tag match", func(t *testing.T) {
		results := ms.Search("", "infra")
		if len(results) != 2 {
			t.Fatalf("got %d results, want 2", len(results))
		}
	})

	t.Run("query AND tag", func(t *testing.T) {
		results := ms.Search("aws", "deploy")
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		if results[0].ID != "m-2" {
			t.Errorf("ID = %s, want m-2", results[0].ID)
		}
	})

	t.Run("no filter returns recent", func(t *testing.T) {
		results := ms.Search("", "")
		if len(results) != 3 {
			t.Fatalf("got %d results, want 3", len(results))
		}
		// Newest first
		if results[0].ID != "m-2" {
			t.Errorf("first result ID = %s, want m-2 (newest)", results[0].ID)
		}
	})

	t.Run("no match", func(t *testing.T) {
		results := ms.Search("nonexistent", "")
		if len(results) != 0 {
			t.Fatalf("got %d results, want 0", len(results))
		}
	})
}

func TestMemoryDelete(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})
	ms.Store("entry one", nil, "")
	ms.Store("entry two", nil, "")
	ms.Store("entry three", nil, "")

	if err := ms.Delete("m-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(ms.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(ms.entries))
	}
	if ms.entries[0].ID != "m-0" || ms.entries[1].ID != "m-2" {
		t.Errorf("remaining IDs = %s, %s — want m-0, m-2", ms.entries[0].ID, ms.entries[1].ID)
	}

	// Verify file was rewritten
	ms2 := NewMemoryStore(dir, MemoryConfig{})
	if len(ms2.entries) != 2 {
		t.Fatalf("reloaded %d entries, want 2", len(ms2.entries))
	}
}

func TestMemoryDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})
	ms.Store("entry", nil, "")

	err := ms.Delete("m-99")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

func TestSecretRejection(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})

	secrets := []string{
		"My API key is sk-proj-abc123def456ghi789jkl012mno345",
		"Token: ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		"Use gho_abcdefghijklmnopqrstuvwxyz1234567890",
		"Auth: glpat-abcdefghij1234567890",
		"Slack token xoxb-1234-5678-abcdefghijklmnop",
		"Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWI",
		"password: mySuperSecret123!",
		"token: abcdefghijklmnopqrstuvwxyz1234",
		// High-entropy blob
		"Key is aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5zA7bC9dE1fG",
	}

	for _, s := range secrets {
		_, err := ms.Store(s, nil, "")
		if err == nil {
			t.Errorf("expected rejection for: %s", s[:30])
		}
	}

	if len(ms.entries) != 0 {
		t.Errorf("stored %d entries, want 0 (all should be rejected)", len(ms.entries))
	}
}

func TestSecretRejectionAllowsNormal(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})

	normal := []string{
		"User prefers Python over JavaScript",
		"The database is PostgreSQL 16",
		"Deploy to us-east-1 on AWS",
		"Use 4-space indentation",
		"API endpoint is /v1/users",
		"The project started in 2024",
	}

	for _, s := range normal {
		_, err := ms.Store(s, nil, "")
		if err != nil {
			t.Errorf("unexpected rejection for %q: %v", s, err)
		}
	}

	if len(ms.entries) != len(normal) {
		t.Errorf("stored %d entries, want %d", len(ms.entries), len(normal))
	}
}

func TestPruneOnStore(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{MaxTotal: 5})

	for i := 0; i < 6; i++ {
		ms.Store("entry "+string(rune('A'+i)), nil, "")
	}

	if len(ms.entries) != 5 {
		t.Fatalf("entries = %d, want 5 (pruned oldest)", len(ms.entries))
	}
	// Oldest (m-0) should be gone
	if ms.entries[0].ID != "m-1" {
		t.Errorf("first entry ID = %s, want m-1 (m-0 pruned)", ms.entries[0].ID)
	}
}

func TestTokenize(t *testing.T) {
	words := tokenize("Hello, World! This is a test of PostgreSQL-16 on port 5432.")
	// "hello" → kept (5 chars)
	// "world" → kept
	// "this" → stopword
	// "is" → too short
	// "a" → too short
	// "test" → kept
	// "of" → too short
	// "postgresql" → kept
	// "16" → too short
	// "on" → too short
	// "port" → kept
	// "5432" → kept

	if !words["hello"] {
		t.Error("missing 'hello'")
	}
	if !words["world"] {
		t.Error("missing 'world'")
	}
	if !words["postgresql"] {
		t.Error("missing 'postgresql'")
	}
	if !words["port"] {
		t.Error("missing 'port'")
	}
	if !words["5432"] {
		t.Error("missing '5432'")
	}
	if words["this"] {
		t.Error("stopword 'this' should be excluded")
	}
	if words["is"] {
		t.Error("short word 'is' should be excluded")
	}
	if words["a"] {
		t.Error("short word 'a' should be excluded")
	}
}

func TestScoreRelevance(t *testing.T) {
	userWords := tokenize("deploy to AWS production")

	entry := MemoryEntry{
		Text: "Deploy target is AWS us-east-1",
		TS:   time.Now().UTC().Format(time.RFC3339),
	}

	score := scoreRelevance(entry, userWords)
	// "deploy" and "aws" overlap → score >= 2
	if score < 2.0 {
		t.Errorf("score = %f, want >= 2.0 (deploy + aws overlap)", score)
	}

	// Non-matching entry
	noMatch := MemoryEntry{
		Text: "User prefers tabs over spaces",
		TS:   time.Now().UTC().Format(time.RFC3339),
	}
	noScore := scoreRelevance(noMatch, userWords)
	if noScore != 0 {
		t.Errorf("no-match score = %f, want 0", noScore)
	}
}

func TestInjectBlockRelevant(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{InjectMode: "relevant"})

	ms.Store("User prefers Python over JavaScript", []string{"preference"}, "user")
	ms.Store("DB is Postgres on port 5432", []string{"infra"}, "workspace")
	ms.Store("Deploy to AWS us-east-1", []string{"deploy"}, "workspace")

	block := ms.InjectBlock("What database should I use?")

	if !strings.Contains(block, "[Memories]") {
		t.Error("missing [Memories] header")
	}
	// "database" matches "Postgres" entry via... actually "database" won't match directly.
	// But the fallback to recent 5 will include all entries.
	if !strings.Contains(block, "m-") {
		t.Error("missing memory IDs in block")
	}
}

func TestInjectBlockRecentOnly(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{InjectMode: "recent_only"})

	for i := 0; i < 10; i++ {
		ms.Store("entry "+string(rune('A'+i)), nil, "")
	}

	block := ms.InjectBlock("anything")

	// Should include last 5
	if !strings.Contains(block, "m-9") {
		t.Error("missing most recent entry m-9")
	}
	if !strings.Contains(block, "m-5") {
		t.Error("missing entry m-5")
	}
	if strings.Contains(block, "m-4") {
		t.Error("should NOT contain m-4 (only last 5)")
	}
}

func TestInjectBlockOff(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{InjectMode: "off"})
	ms.Store("some memory", nil, "")

	block := ms.InjectBlock("anything")
	if block != "" {
		t.Errorf("inject_mode=off should return empty, got %q", block)
	}
}

func TestInjectBlockBudget(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{
		InjectMode:     "recent_only",
		MaxInjectChars: 50,
		MaxInjectCount: 2,
	})

	ms.Store("short", nil, "")
	ms.Store("another short entry", nil, "")
	ms.Store("this is a much longer entry that should push us over the char budget limit easily", nil, "")
	ms.Store("fourth entry", nil, "")
	ms.Store("fifth entry", nil, "")

	block := ms.InjectBlock("anything")
	lines := strings.Split(strings.TrimSpace(block), "\n")
	// First line is "[Memories]", rest are entries
	entryLines := lines[1:]
	if len(entryLines) > 2 {
		t.Errorf("got %d entry lines, want <= 2 (max_inject_count)", len(entryLines))
	}
}

func TestInjectBlockEmpty(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})

	block := ms.InjectBlock("anything")
	if block != "" {
		t.Errorf("empty store should return empty block, got %q", block)
	}
}

func TestMemoryStoreMaxLength(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})

	longText := strings.Repeat("x", 501)
	_, err := ms.Store(longText, nil, "")
	if err == nil {
		t.Fatal("expected error for text > 500 chars")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want mention of 500 char limit", err.Error())
	}
}

func TestInjectBlockFormatsWithTags(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{InjectMode: "recent_only"})
	ms.Store("Python preferred", []string{"preference"}, "user")
	ms.Store("No tags here", nil, "")

	block := ms.InjectBlock("anything")

	if !strings.Contains(block, "(m-0, preference)") {
		t.Errorf("missing tag in format: %s", block)
	}
	if !strings.Contains(block, "(m-1)") {
		t.Errorf("missing no-tag format: %s", block)
	}
}

func TestMemoryFileAtomicDelete(t *testing.T) {
	dir := t.TempDir()
	ms := NewMemoryStore(dir, MemoryConfig{})
	ms.Store("keep", nil, "")
	ms.Store("delete me", nil, "")
	ms.Store("keep too", nil, "")

	ms.Delete("m-1")

	// Verify no temp file left behind
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// Verify file contents
	data, _ := os.ReadFile(filepath.Join(dir, memoriesFile))
	if strings.Contains(string(data), "delete me") {
		t.Error("deleted entry still in file")
	}
	if !strings.Contains(string(data), "keep") || !strings.Contains(string(data), "keep too") {
		t.Error("remaining entries missing from file")
	}
}
