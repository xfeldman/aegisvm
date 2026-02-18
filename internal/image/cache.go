package image

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Cache provides digest-keyed caching for unpacked OCI image rootfs directories.
// Cache layout: {cacheDir}/sha256_{digest}/  — unpacked rootfs.
type Cache struct {
	mu       sync.Mutex
	cacheDir string
}

// NewCache creates a new image cache.
func NewCache(cacheDir string) *Cache {
	return &Cache{cacheDir: cacheDir}
}

// GetOrPull returns the path to the unpacked rootfs for the given image reference.
// If the image is already cached (by digest), the cached path is returned.
// Otherwise, the image is pulled, unpacked, and cached.
func (c *Cache) GetOrPull(ctx context.Context, imageRef string) (rootfsDir string, digest string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Pull to get the digest (this is a remote HEAD + manifest fetch, not full pull if cached)
	log.Printf("image: resolving %s", imageRef)
	result, err := Pull(ctx, imageRef)
	if err != nil {
		return "", "", fmt.Errorf("pull %s: %w", imageRef, err)
	}

	digest = result.Digest
	dirName := digestToDirName(digest)
	cachedDir := filepath.Join(c.cacheDir, dirName)

	// Check if already cached
	if _, err := os.Stat(cachedDir); err == nil {
		log.Printf("image: cache hit for %s (%s)", imageRef, digest)
		return cachedDir, digest, nil
	}

	// Unpack into cache
	log.Printf("image: unpacking %s (%s)", imageRef, digest)
	tmpDir := cachedDir + ".tmp"
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", "", fmt.Errorf("create tmp dir: %w", err)
	}

	if err := Unpack(result.Image, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("unpack %s: %w", imageRef, err)
	}

	// Atomic rename
	if err := os.Rename(tmpDir, cachedDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("rename cache dir: %w", err)
	}

	log.Printf("image: cached %s at %s", imageRef, cachedDir)
	return cachedDir, digest, nil
}

// InjectHarness copies the harness binary into the rootfs at /usr/bin/aegis-harness.
// This path must match the ExecPath in the vmm-worker config — libkrun's
// krun_set_exec() always starts /usr/bin/aegis-harness as PID 1, regardless
// of the OCI image's ENTRYPOINT or CMD. Any existing file at this path in
// the image is intentionally overwritten.
func InjectHarness(rootfsDir, harnessBin string) error {
	destPath := filepath.Join(rootfsDir, "usr", "bin", "aegis-harness")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create harness dir: %w", err)
	}

	src, err := os.Open(harnessBin)
	if err != nil {
		return fmt.Errorf("open harness binary: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create harness in rootfs: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy harness: %w", err)
	}

	return nil
}

// digestToDirName converts a digest like "sha256:abc123" to "sha256_abc123".
func digestToDirName(digest string) string {
	return strings.Replace(digest, ":", "_", 1)
}
