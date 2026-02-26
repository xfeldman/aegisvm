package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCronStoreCreateAndList(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)

	id1, err := cs.Create("*/5 * * * *", "health check", "health", "skip")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	id2, err := cs.Create("0 9 * * *", "daily report", "", "queue")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	id3, err := cs.Create("0 0 1 * *", "monthly cleanup", "monthly", "")
	if err != nil {
		t.Fatalf("Create 3: %v", err)
	}

	if id1 != "cron-0" || id2 != "cron-1" || id3 != "cron-2" {
		t.Errorf("IDs = %s, %s, %s — want cron-0, cron-1, cron-2", id1, id2, id3)
	}

	entries, err := cs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Message != "health check" {
		t.Errorf("entry 0 message = %q", entries[0].Message)
	}
	if entries[0].Enabled != true {
		t.Error("entry 0 should be enabled by default")
	}
	if entries[0].OnConflict != "skip" {
		t.Errorf("entry 0 on_conflict = %q, want 'skip'", entries[0].OnConflict)
	}
}

func TestCronStoreDelete(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)

	cs.Create("* * * * *", "one", "", "")
	cs.Create("* * * * *", "two", "", "")
	cs.Create("* * * * *", "three", "", "")

	if err := cs.Delete("cron-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, _ := cs.List()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ID != "cron-0" || entries[1].ID != "cron-2" {
		t.Errorf("remaining IDs = %s, %s", entries[0].ID, entries[1].ID)
	}

	// Verify persisted
	cs2 := NewCronStore(dir)
	entries2, _ := cs2.List()
	if len(entries2) != 2 {
		t.Fatalf("reloaded entries = %d, want 2", len(entries2))
	}
}

func TestCronStoreEnableDisable(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)
	cs.Create("* * * * *", "test", "", "")

	// Disable
	if err := cs.SetEnabled("cron-0", false); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	entries, _ := cs.List()
	if entries[0].Enabled {
		t.Error("should be disabled")
	}

	// Enable
	if err := cs.SetEnabled("cron-0", true); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	entries, _ = cs.List()
	if !entries[0].Enabled {
		t.Error("should be enabled")
	}
}

func TestCronStoreMaxEntries(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)

	for i := 0; i < 20; i++ {
		_, err := cs.Create("* * * * *", "entry", "", "")
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	_, err := cs.Create("* * * * *", "one too many", "", "")
	if err == nil {
		t.Fatal("expected error for 21st entry")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error = %q, want 'maximum' mention", err.Error())
	}
}

func TestCronStoreInvalidSchedule(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)

	tests := []string{
		"not a cron",
		"* * *",
		"60 * * * *",
		"@daily",
	}

	for _, sched := range tests {
		_, err := cs.Create(sched, "test", "", "")
		if err == nil {
			t.Errorf("expected error for schedule %q", sched)
		}
	}
}

func TestCronStoreAtomicSave(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)
	cs.Create("* * * * *", "test", "", "")

	// No temp files should remain
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// File should be valid JSON
	data, _ := os.ReadFile(filepath.Join(dir, "cron.json"))
	var cf CronFile
	if err := json.Unmarshal(data, &cf); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(cf.Entries) != 1 {
		t.Errorf("entries = %d, want 1", len(cf.Entries))
	}
}

func TestCronStoreDefaultSession(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)
	cs.Create("* * * * *", "test", "", "")

	entries, _ := cs.List()
	if entries[0].Session != "cron-0" {
		t.Errorf("session = %q, want 'cron-0' (default)", entries[0].Session)
	}
}

func TestCronStoreDefaultOnConflict(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)
	cs.Create("* * * * *", "test", "", "")

	entries, _ := cs.List()
	if entries[0].OnConflict != "skip" {
		t.Errorf("on_conflict = %q, want 'skip' (default)", entries[0].OnConflict)
	}
}

func TestCronStoreDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)
	cs.Create("* * * * *", "test", "", "")

	err := cs.Delete("cron-99")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

func TestCronStoreInvalidOnConflict(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronStore(dir)

	_, err := cs.Create("* * * * *", "test", "", "invalid")
	if err == nil {
		t.Fatal("expected error for invalid on_conflict")
	}
}
