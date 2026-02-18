package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "super-secret-api-key-12345"
	ct, err := s.EncryptString(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.DecryptString(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != plaintext {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestRoundTripBytes(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	data := []byte{0, 1, 2, 3, 255, 254, 253}
	ct, err := s.Encrypt(data)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(data))
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("byte %d: got %d, want %d", i, got[i], data[i])
		}
	}
}

func TestTamperDetection(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	ct, err := s.EncryptString("hello")
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte in the ciphertext
	ct[len(ct)-1] ^= 0xFF

	_, err = s.DecryptString(ct)
	if err == nil {
		t.Fatal("expected tamper detection error")
	}
}

func TestKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")

	s1, err := NewStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	ct, err := s1.EncryptString("persist-test")
	if err != nil {
		t.Fatal(err)
	}

	// Create a new store from the same key file
	s2, err := NewStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s2.DecryptString(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != "persist-test" {
		t.Fatalf("got %q, want %q", got, "persist-test")
	}
}

func TestKeyAutoGeneration(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "subdir", "master.key")

	_, err := NewStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != masterKeyLen {
		t.Fatalf("key length: got %d, want %d", len(data), masterKeyLen)
	}

	// Check file permissions
	info, _ := os.Stat(keyPath)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: got %o, want 0600", info.Mode().Perm())
	}
}

func TestNonceUniqueness(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt the same plaintext twice — ciphertexts should differ
	ct1, _ := s.EncryptString("same")
	ct2, _ := s.EncryptString("same")

	if len(ct1) == len(ct2) {
		same := true
		for i := range ct1 {
			if ct1[i] != ct2[i] {
				same = false
				break
			}
		}
		if same {
			t.Fatal("two encryptions of same plaintext produced identical ciphertext")
		}
	}
}

func TestInvalidKeyLength(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "bad.key")
	os.WriteFile(keyPath, []byte("short"), 0600)

	_, err := NewStore(keyPath)
	if err == nil {
		t.Fatal("expected error for invalid key length")
	}
}

func TestEmptyPlaintext(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	ct, err := s.EncryptString("")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.DecryptString(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}
