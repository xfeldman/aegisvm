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
//
// A local ref→digest index avoids hitting the registry on every boot.
// The index is populated on first pull and reused for subsequent lookups.
type Cache struct {
	mu       sync.Mutex
	cacheDir string
	refIndex map[string]string // imageRef → digest (in-memory, rebuilt from disk on miss)
}

// NewCache creates a new image cache.
func NewCache(cacheDir string) *Cache {
	return &Cache{
		cacheDir: cacheDir,
		refIndex: make(map[string]string),
	}
}

// GetOrPull returns the path to the unpacked rootfs for the given image reference.
// If the image is already cached (by digest), the cached path is returned
// without any network calls. Otherwise, the image is pulled, unpacked, and cached.
func (c *Cache) GetOrPull(ctx context.Context, imageRef string) (rootfsDir string, digest string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fast path: check ref→digest index (no network)
	if d, ok := c.refIndex[imageRef]; ok {
		cachedDir := filepath.Join(c.cacheDir, digestToDirName(d))
		if _, err := os.Stat(cachedDir); err == nil {
			log.Printf("image: local cache hit for %s (%s)", imageRef, d)
			return cachedDir, d, nil
		}
		// Index entry stale (dir deleted), fall through to pull
		delete(c.refIndex, imageRef)
	}

	// Scan disk for existing cache entries if index is empty
	// (handles daemon restart where in-memory index is lost)
	if len(c.refIndex) == 0 {
		c.rebuildIndex()
		if d, ok := c.refIndex[imageRef]; ok {
			cachedDir := filepath.Join(c.cacheDir, digestToDirName(d))
			if _, err := os.Stat(cachedDir); err == nil {
				log.Printf("image: disk cache hit for %s (%s)", imageRef, d)
				return cachedDir, d, nil
			}
		}
	}

	// Pull to get the digest (remote HEAD + manifest fetch)
	log.Printf("image: resolving %s (network)", imageRef)
	result, err := Pull(ctx, imageRef)
	if err != nil {
		return "", "", fmt.Errorf("pull %s: %w", imageRef, err)
	}

	digest = result.Digest
	dirName := digestToDirName(digest)
	cachedDir := filepath.Join(c.cacheDir, dirName)

	// Update index
	c.refIndex[imageRef] = digest

	// Check if already cached (by digest)
	if _, err := os.Stat(cachedDir); err == nil {
		log.Printf("image: cache hit for %s (%s)", imageRef, digest)
		// Write ref file for future index rebuilds
		c.writeRefFile(cachedDir, imageRef)
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

	// Write ref file for future index rebuilds
	c.writeRefFile(cachedDir, imageRef)

	log.Printf("image: cached %s at %s", imageRef, cachedDir)
	return cachedDir, digest, nil
}

// writeRefFile stores the imageRef inside the cached dir so rebuildIndex can map it back.
func (c *Cache) writeRefFile(cachedDir, imageRef string) {
	os.WriteFile(filepath.Join(cachedDir, ".image-ref"), []byte(imageRef), 0644)
}

// rebuildIndex scans the cache directory and rebuilds ref→digest from .image-ref files.
func (c *Cache) rebuildIndex() {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		refFile := filepath.Join(c.cacheDir, e.Name(), ".image-ref")
		data, err := os.ReadFile(refFile)
		if err != nil {
			continue
		}
		ref := strings.TrimSpace(string(data))
		digest := strings.Replace(e.Name(), "_", ":", 1) // sha256_abc → sha256:abc
		c.refIndex[ref] = digest
	}
	if len(c.refIndex) > 0 {
		log.Printf("image: rebuilt index from disk (%d entries)", len(c.refIndex))
	}
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
