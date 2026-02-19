package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRingBufferEvictionByCount(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-1", "app-1", "rel-1")

	// Fill ring buffer beyond maxLines
	for i := 0; i < maxLines+100; i++ {
		il.Append("stdout", "line", "")
	}

	entries := il.Read(time.Time{}, 0)
	if len(entries) != maxLines {
		t.Fatalf("expected %d entries, got %d", maxLines, len(entries))
	}
}

func TestRingBufferEvictionByBytes(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-2", "", "")

	// Write entries with large lines to exceed byte cap
	bigLine := strings.Repeat("x", 10000)
	for i := 0; i < 1000; i++ {
		il.Append("stdout", bigLine, "")
	}

	entries := il.Read(time.Time{}, 0)
	totalBytes := 0
	for _, e := range entries {
		totalBytes += len(e.Line) + len(e.Stream) + len(e.ExecID) + 100
	}
	if totalBytes > maxBytes+20000 {
		t.Fatalf("ring buffer bytes %d exceeded max %d by too much", totalBytes, maxBytes)
	}
}

func TestFilePersistence(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-3", "app-1", "rel-1")

	il.Append("stdout", "hello", "")
	il.Append("stderr", "world", "")

	// Check file exists and has content
	filePath := filepath.Join(dir, "inst-3.ndjson")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Fatal("log file does not contain 'hello'")
	}
	if !strings.Contains(string(data), "world") {
		t.Fatal("log file does not contain 'world'")
	}
}

func TestFileRotation(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-4", "", "")

	// Write enough data to trigger rotation
	bigLine := strings.Repeat("a", 100000) // 100KB per line
	for i := 0; i < 120; i++ { // ~12MB total > maxFileBytes
		il.Append("stdout", bigLine, "")
	}

	// Check that rotation happened
	rotatedPath := filepath.Join(dir, "inst-4.ndjson.1")
	if _, err := os.Stat(rotatedPath); os.IsNotExist(err) {
		t.Fatal("rotated log file does not exist")
	}
}

func TestSubscribeAndRead(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-5", "", "")

	// Add some entries before subscribing
	il.Append("stdout", "before-1", "")
	il.Append("stdout", "before-2", "")

	ch, existing, unsub := il.Subscribe()
	defer unsub()

	if len(existing) != 2 {
		t.Fatalf("expected 2 existing entries, got %d", len(existing))
	}

	// Add entry after subscribing
	il.Append("stdout", "after-1", "")

	select {
	case entry := <-ch:
		if entry.Line != "after-1" {
			t.Fatalf("expected 'after-1', got %q", entry.Line)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription entry")
	}
}

func TestReadSinceAndTail(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-6", "", "")

	t1 := time.Now()
	time.Sleep(10 * time.Millisecond)
	il.Append("stdout", "line-1", "")
	il.Append("stdout", "line-2", "")
	il.Append("stdout", "line-3", "")
	il.Append("stdout", "line-4", "")

	// Read all
	all := il.Read(time.Time{}, 0)
	if len(all) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(all))
	}

	// Read since t1
	since := il.Read(t1, 0)
	if len(since) != 4 {
		t.Fatalf("expected 4 entries since t1, got %d", len(since))
	}

	// Read tail 2
	tail := il.Read(time.Time{}, 2)
	if len(tail) != 2 {
		t.Fatalf("expected 2 tail entries, got %d", len(tail))
	}
	if tail[0].Line != "line-3" || tail[1].Line != "line-4" {
		t.Fatalf("unexpected tail entries: %v, %v", tail[0].Line, tail[1].Line)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	il := s.GetOrCreate("inst-7", "", "")
	il.Append("stdout", "test", "")

	filePath := filepath.Join(dir, "inst-7.ndjson")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatal("log file should exist")
	}

	s.Remove("inst-7")

	if s.Get("inst-7") != nil {
		t.Fatal("instance log should be nil after Remove")
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("log file should be removed")
	}
}

func TestGetOrCreateIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	il1 := s.GetOrCreate("inst-8", "app-1", "rel-1")
	il2 := s.GetOrCreate("inst-8", "app-1", "rel-1")

	if il1 != il2 {
		t.Fatal("GetOrCreate should return the same InstanceLog for the same ID")
	}
}
