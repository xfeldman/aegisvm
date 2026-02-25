package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutGetRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	data := []byte("hello world png")
	key, err := store.Put(data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if !strings.HasSuffix(key, ".png") {
		t.Errorf("expected .png suffix, got %q", key)
	}

	got, err := store.Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("roundtrip mismatch: got %q, want %q", got, data)
	}
}

func TestContentAddressedDedup(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	data := []byte("dedup test data")
	key1, err := store.Put(data, "image/jpeg")
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	key2, err := store.Put(data, "image/jpeg")
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if key1 != key2 {
		t.Errorf("dedup failed: key1=%s key2=%s", key1, key2)
	}

	// Verify only one file exists
	blobDir := filepath.Join(dir, ".aegis", "blobs")
	entries, _ := os.ReadDir(blobDir)
	count := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".tmp-") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 blob file, got %d", count)
	}
}

func TestKeyValidationPathTraversal(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	badKeys := []string{
		"../../etc/passwd",
		"../foo.png",
		"abc.png",                   // too short
		"zzzzzzzzzzzzzzzz.png",      // not 64 hex chars
		strings.Repeat("a", 64) + ".exe", // bad extension
		"",
	}
	for _, key := range badKeys {
		_, err := store.Get(key)
		if err == nil {
			t.Errorf("expected error for key %q, got nil", key)
		}
	}
}

func TestOversizedRejection(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	data := make([]byte, MaxImageBytes+1)
	_, err := store.Put(data, "image/png")
	if err == nil {
		t.Fatal("expected error for oversized image")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMissingBlob(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	key := strings.Repeat("a", 64) + ".png"
	_, err := store.Get(key)
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestExtMapping(t *testing.T) {
	tests := []struct {
		mediaType string
		ext       string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"image/bmp", ""},
		{"text/plain", ""},
	}
	for _, tt := range tests {
		got := extForMediaType(tt.mediaType)
		if got != tt.ext {
			t.Errorf("extForMediaType(%q) = %q, want %q", tt.mediaType, got, tt.ext)
		}
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	_, err := store.Put([]byte("data"), "image/bmp")
	if err == nil {
		t.Fatal("expected error for unsupported media type")
	}
}

func TestPutKeyFormat(t *testing.T) {
	dir := t.TempDir()
	store := NewWorkspaceBlobStore(dir)

	data := []byte("test key format")
	key, err := store.Put(data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify key matches expected hash
	hash := sha256.Sum256(data)
	expected := hex.EncodeToString(hash[:]) + ".png"
	if key != expected {
		t.Errorf("key = %q, want %q", key, expected)
	}

	// Verify key matches validation regex
	if !validBlobKey.MatchString(key) {
		t.Errorf("key %q does not match validBlobKey regex", key)
	}
}
