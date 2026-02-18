package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInjectHarness_Success(t *testing.T) {
	rootfsDir := t.TempDir()
	harnessDir := t.TempDir()

	// Create a fake harness binary
	harnessBin := filepath.Join(harnessDir, "aegis-harness")
	harnessContent := []byte("#!/bin/sh\necho harness\n")
	if err := os.WriteFile(harnessBin, harnessContent, 0755); err != nil {
		t.Fatalf("write harness binary: %v", err)
	}

	if err := InjectHarness(rootfsDir, harnessBin); err != nil {
		t.Fatalf("InjectHarness: %v", err)
	}

	// Verify the harness was copied to the correct path
	destPath := filepath.Join(rootfsDir, "usr", "bin", "aegis-harness")
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read injected harness: %v", err)
	}
	if string(data) != string(harnessContent) {
		t.Errorf("injected content = %q, want %q", data, harnessContent)
	}

	// Verify the file is executable
	info, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("stat injected harness: %v", err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Errorf("harness should be executable, got mode %v", info.Mode())
	}
}

func TestInjectHarness_CreatesDirectories(t *testing.T) {
	rootfsDir := t.TempDir()
	harnessDir := t.TempDir()

	// Create a fake harness binary
	harnessBin := filepath.Join(harnessDir, "aegis-harness")
	if err := os.WriteFile(harnessBin, []byte("binary"), 0755); err != nil {
		t.Fatalf("write harness binary: %v", err)
	}

	// rootfsDir/usr/bin does not exist yet — InjectHarness should create it
	if err := InjectHarness(rootfsDir, harnessBin); err != nil {
		t.Fatalf("InjectHarness: %v", err)
	}

	// Verify intermediate directories were created
	info, err := os.Stat(filepath.Join(rootfsDir, "usr", "bin"))
	if err != nil {
		t.Fatalf("stat usr/bin: %v", err)
	}
	if !info.IsDir() {
		t.Error("usr/bin should be a directory")
	}
}

func TestInjectHarness_OverwritesExisting(t *testing.T) {
	rootfsDir := t.TempDir()
	harnessDir := t.TempDir()

	// Pre-create the destination with old content
	destDir := filepath.Join(rootfsDir, "usr", "bin")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(destDir, "aegis-harness")
	if err := os.WriteFile(destPath, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a new harness binary
	harnessBin := filepath.Join(harnessDir, "aegis-harness")
	newContent := []byte("new-binary-content")
	if err := os.WriteFile(harnessBin, newContent, 0755); err != nil {
		t.Fatal(err)
	}

	if err := InjectHarness(rootfsDir, harnessBin); err != nil {
		t.Fatalf("InjectHarness: %v", err)
	}

	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read harness: %v", err)
	}
	if string(data) != string(newContent) {
		t.Errorf("content = %q, want %q (should overwrite existing)", data, newContent)
	}
}

func TestInjectHarness_SourceNotFound(t *testing.T) {
	rootfsDir := t.TempDir()

	err := InjectHarness(rootfsDir, "/nonexistent/path/aegis-harness")
	if err == nil {
		t.Fatal("expected error for missing source binary, got nil")
	}
}

func TestInjectHarness_LargeFile(t *testing.T) {
	rootfsDir := t.TempDir()
	harnessDir := t.TempDir()

	// Create a larger fake binary (1 MB) to test copy correctness
	harnessBin := filepath.Join(harnessDir, "aegis-harness")
	largeContent := make([]byte, 1024*1024)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}
	if err := os.WriteFile(harnessBin, largeContent, 0755); err != nil {
		t.Fatalf("write harness binary: %v", err)
	}

	if err := InjectHarness(rootfsDir, harnessBin); err != nil {
		t.Fatalf("InjectHarness: %v", err)
	}

	destPath := filepath.Join(rootfsDir, "usr", "bin", "aegis-harness")
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read injected harness: %v", err)
	}
	if len(data) != len(largeContent) {
		t.Errorf("copied size = %d, want %d", len(data), len(largeContent))
	}
	// Spot check a few bytes
	for _, idx := range []int{0, 1023, 512*1024, len(largeContent) - 1} {
		if data[idx] != largeContent[idx] {
			t.Errorf("byte at offset %d = %d, want %d", idx, data[idx], largeContent[idx])
		}
	}
}

func TestDigestToDirName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sha256:abc123def456", "sha256_abc123def456"},
		{"sha512:xyz789", "sha512_xyz789"},
		{"nocolon", "nocolon"},
		{"multi:colon:digest", "multi_colon:digest"}, // only first colon replaced
	}

	for _, tt := range tests {
		got := digestToDirName(tt.input)
		if got != tt.want {
			t.Errorf("digestToDirName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
