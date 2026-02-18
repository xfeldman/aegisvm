package overlay

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CopyOverlay implements Overlay using full directory copies.
// It uses a tar pipe (tar c | tar x) to preserve symlinks, which macOS cp
// can break for busybox-style rootfs layouts (spec §7.6).
type CopyOverlay struct {
	baseDir string // directory containing all overlay copies
}

// NewCopyOverlay creates a CopyOverlay that stores copies under baseDir.
func NewCopyOverlay(baseDir string) *CopyOverlay {
	return &CopyOverlay{baseDir: baseDir}
}

func (o *CopyOverlay) Create(ctx context.Context, sourceDir, destID string) (string, error) {
	dest := filepath.Join(o.baseDir, destID)

	if err := os.MkdirAll(dest, 0755); err != nil {
		return "", fmt.Errorf("create dest dir: %w", err)
	}

	// Use tar pipe to preserve symlinks and permissions.
	// tar -C source -cf - . | tar -C dest -xf -
	tarCreate := exec.CommandContext(ctx, "tar", "-C", sourceDir, "-cf", "-", ".")
	tarExtract := exec.CommandContext(ctx, "tar", "-C", dest, "-xf", "-")

	pipe, err := tarCreate.StdoutPipe()
	if err != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("tar stdout pipe: %w", err)
	}
	tarExtract.Stdin = pipe

	if err := tarCreate.Start(); err != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("start tar create: %w", err)
	}
	if err := tarExtract.Start(); err != nil {
		tarCreate.Process.Kill()
		tarCreate.Wait()
		os.RemoveAll(dest)
		return "", fmt.Errorf("start tar extract: %w", err)
	}

	createErr := tarCreate.Wait()
	extractErr := tarExtract.Wait()
	if createErr != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("tar create: %w", createErr)
	}
	if extractErr != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("tar extract: %w", extractErr)
	}

	return dest, nil
}

func (o *CopyOverlay) Remove(id string) error {
	return os.RemoveAll(filepath.Join(o.baseDir, id))
}

func (o *CopyOverlay) Path(id string) string {
	return filepath.Join(o.baseDir, id)
}

// CleanStaleTasks removes task-* overlay directories older than maxAge.
// Called on daemon startup to clean up after crashes.
func (o *CopyOverlay) CleanStaleTasks(maxAge time.Duration) {
	entries, err := os.ReadDir(o.baseDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "task-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(o.baseDir, e.Name())
			log.Printf("overlay GC: removing stale task overlay %s (age=%v)", e.Name(), time.Since(info.ModTime()).Round(time.Minute))
			os.RemoveAll(path)
		}
	}
}
