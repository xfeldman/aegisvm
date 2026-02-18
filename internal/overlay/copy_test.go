package overlay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyOverlayCreateAndRemove(t *testing.T) {
	sourceDir := t.TempDir()
	baseDir := t.TempDir()

	// Create a source tree with a regular file and a symlink
	if err := os.WriteFile(filepath.Join(sourceDir, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "subdir", "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	ov := NewCopyOverlay(baseDir)

	// Create
	dest, err := ov.Create(context.Background(), sourceDir, "test-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify path
	if dest != ov.Path("test-1") {
		t.Errorf("dest = %q, want %q", dest, ov.Path("test-1"))
	}

	// Verify regular file
	data, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("hello.txt = %q, want %q", data, "hello")
	}

	// Verify nested file
	data, err = os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("nested.txt = %q, want %q", data, "nested")
	}

	// Verify symlink is preserved (not dereferenced)
	target, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "hello.txt" {
		t.Errorf("symlink target = %q, want %q", target, "hello.txt")
	}

	// Remove
	if err := ov.Remove("test-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("directory still exists after remove")
	}
}

func TestCleanStale(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	// Create a stale task dir (fake old modtime)
	staleDir := filepath.Join(baseDir, "task-task-1")
	os.MkdirAll(staleDir, 0755)
	os.WriteFile(filepath.Join(staleDir, "file"), []byte("x"), 0644)
	oldTime := time.Now().Add(-3 * time.Hour)
	os.Chtimes(staleDir, oldTime, oldTime)

	// Create a fresh task dir
	freshDir := filepath.Join(baseDir, "task-task-2")
	os.MkdirAll(freshDir, 0755)
	os.WriteFile(filepath.Join(freshDir, "file"), []byte("x"), 0644)

	// Create a release dir (not a task — should not be cleaned)
	releaseDir := filepath.Join(baseDir, "rel-123")
	os.MkdirAll(releaseDir, 0755)
	oldTime2 := time.Now().Add(-5 * time.Hour)
	os.Chtimes(releaseDir, oldTime2, oldTime2)

	// Create an incomplete staging dir (.tmp — always removed)
	stagingDir := filepath.Join(baseDir, "rel-456.tmp")
	os.MkdirAll(stagingDir, 0755)
	os.WriteFile(filepath.Join(stagingDir, "file"), []byte("x"), 0644)

	// Run GC with 1 hour max age
	ov.CleanStale(1 * time.Hour)

	// Stale task dir should be removed
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale task dir should have been removed")
	}

	// Fresh task dir should still exist
	if _, err := os.Stat(freshDir); err != nil {
		t.Error("fresh task dir should still exist")
	}

	// Release dir should still exist (not a task-*)
	if _, err := os.Stat(releaseDir); err != nil {
		t.Error("release dir should not be cleaned by task GC")
	}

	// Staging dir should always be removed regardless of age
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("staging .tmp dir should have been removed")
	}
}

func TestCreateUsesAtomicRename(t *testing.T) {
	sourceDir := t.TempDir()
	baseDir := t.TempDir()
	os.WriteFile(filepath.Join(sourceDir, "data.txt"), []byte("content"), 0644)

	ov := NewCopyOverlay(baseDir)

	dest, err := ov.Create(context.Background(), sourceDir, "rel-test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Final dir should exist
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("final dir missing: %v", err)
	}

	// Staging dir should NOT exist (renamed away)
	staging := dest + ".tmp"
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Error("staging .tmp dir should not exist after successful create")
	}

	// Content should be correct
	data, err := os.ReadFile(filepath.Join(dest, "data.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("data = %q, want %q", data, "content")
	}
}

func TestCopyOverlayRemoveNonexistent(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	// Should not error on nonexistent
	if err := ov.Remove("nonexistent"); err != nil {
		t.Fatalf("remove nonexistent: %v", err)
	}
}
