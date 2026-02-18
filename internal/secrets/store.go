// Package secrets provides AES-256-GCM encryption for secret values.
// The master key is stored at a configurable path (default ~/.aegis/master.key)
// and auto-generated on first use.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

const masterKeyLen = 32 // AES-256

// Store provides encrypt/decrypt operations using a persisted master key.
type Store struct {
	masterKey []byte
	keyPath   string
}

// NewStore loads the master key from keyPath, or generates one if it doesn't exist.
func NewStore(keyPath string) (*Store, error) {
	s := &Store{keyPath: keyPath}

	data, err := os.ReadFile(keyPath)
	if err == nil {
		if len(data) != masterKeyLen {
			return nil, fmt.Errorf("master key at %s has invalid length %d (expected %d)", keyPath, len(data), masterKeyLen)
		}
		s.masterKey = data
		return s, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	// Generate new key
	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}

	s.masterKey = key
	return s, nil
}

// Encrypt encrypts plaintext using AES-256-GCM. Returns nonce || ciphertext.
func (s *Store) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts data produced by Encrypt (nonce || ciphertext).
func (s *Store) Decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// EncryptString encrypts a string value.
func (s *Store) EncryptString(value string) ([]byte, error) {
	return s.Encrypt([]byte(value))
}

// DecryptString decrypts to a string value.
func (s *Store) DecryptString(ciphertext []byte) (string, error) {
	plaintext, err := s.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
