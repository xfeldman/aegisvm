// Package blob provides content-addressed blob storage for tether image support.
// v1 implementation: workspace filesystem ({root}/.aegis/blobs/{sha256}.{ext}).
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	// MaxImageBytes is the maximum size of a single image (10 MB).
	MaxImageBytes = 10 << 20

	// MaxImagesPerMessage is the maximum number of images per tether message.
	MaxImagesPerMessage = 4
)

// validBlobKey matches keys produced by Put: 64 hex chars + known extension.
var validBlobKey = regexp.MustCompile(`^[a-f0-9]{64}\.(png|jpg|gif|webp)$`)

// BlobStore is the interface for image blob storage.
// v1 implementation: workspace filesystem.
// Future: object store, content-addressed remote, etc.
type BlobStore interface {
	Put(data []byte, mediaType string) (key string, err error)
	Get(key string) ([]byte, error)
}

// WorkspaceBlobStore stores blobs in {root}/.aegis/blobs/{sha256}.{ext}.
type WorkspaceBlobStore struct {
	root string
}

// NewWorkspaceBlobStore creates a new blob store rooted at the given workspace path.
func NewWorkspaceBlobStore(root string) *WorkspaceBlobStore {
	return &WorkspaceBlobStore{root: root}
}

// Put writes image data to the blob store and returns a content-addressed key.
func (s *WorkspaceBlobStore) Put(data []byte, mediaType string) (string, error) {
	if len(data) > MaxImageBytes {
		return "", fmt.Errorf("image too large: %d bytes (max %d)", len(data), MaxImageBytes)
	}

	ext := extForMediaType(mediaType)
	if ext == "" {
		return "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	hash := sha256.Sum256(data)
	key := hex.EncodeToString(hash[:]) + ext
	dir := filepath.Join(s.root, ".aegis", "blobs")
	final := filepath.Join(dir, key)

	// Content-addressed dedup: skip if exists
	if _, err := os.Stat(final); err == nil {
		return key, nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Atomic write: temp file then rename (prevents partial files on crash)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return key, nil
}

// Get reads a blob by key. The key is validated to prevent path traversal.
func (s *WorkspaceBlobStore) Get(key string) ([]byte, error) {
	if !validBlobKey.MatchString(key) {
		return nil, fmt.Errorf("invalid blob key: %q", key)
	}
	path := filepath.Join(s.root, ".aegis", "blobs", key)
	return os.ReadFile(path)
}

// extForMediaType returns the file extension for a given MIME type.
func extForMediaType(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}
