package overlay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

func TestCopyOverlayRemoveNonexistent(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	// Should not error on nonexistent
	if err := ov.Remove("nonexistent"); err != nil {
		t.Fatalf("remove nonexistent: %v", err)
	}
}
