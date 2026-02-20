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

	// Reuse existing overlay if present (e.g. after daemon restart)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	staging := dest + ".tmp"

	// Clean up any leftover staging dir from a previous crash
	os.RemoveAll(staging)

	if err := os.MkdirAll(staging, 0755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}

	// Use tar pipe to preserve symlinks and permissions.
	// tar -C source -cf - . | tar -C staging -xf -
	tarCreate := exec.CommandContext(ctx, "tar", "-C", sourceDir, "-cf", "-", ".")
	tarExtract := exec.CommandContext(ctx, "tar", "-C", staging, "-xf", "-")

	pipe, err := tarCreate.StdoutPipe()
	if err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("tar stdout pipe: %w", err)
	}
	tarExtract.Stdin = pipe

	if err := tarCreate.Start(); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("start tar create: %w", err)
	}
	if err := tarExtract.Start(); err != nil {
		tarCreate.Process.Kill()
		tarCreate.Wait()
		os.RemoveAll(staging)
		return "", fmt.Errorf("start tar extract: %w", err)
	}

	createErr := tarCreate.Wait()
	extractErr := tarExtract.Wait()
	if createErr != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("tar create: %w", createErr)
	}
	if extractErr != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("tar extract: %w", extractErr)
	}

	// Atomic rename from staging to final destination.
	// If this fails, the staging dir is cleaned up by the caller or
	// by CleanStaleTasks on next daemon startup.
	if err := os.Rename(staging, dest); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("rename staging to final: %w", err)
	}

	return dest, nil
}

func (o *CopyOverlay) Remove(id string) error {
	return os.RemoveAll(filepath.Join(o.baseDir, id))
}

func (o *CopyOverlay) Path(id string) string {
	return filepath.Join(o.baseDir, id)
}

// CleanStale removes stale overlay directories on daemon startup:
//   - task-* directories older than maxAge (crashed task cleanups)
//   - *.tmp directories of any age (incomplete staging from crashed publishes)
func (o *CopyOverlay) CleanStale(maxAge time.Duration) {
	entries, err := os.ReadDir(o.baseDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(o.baseDir, name)

		// Always remove incomplete staging directories
		if strings.HasSuffix(name, ".tmp") {
			log.Printf("overlay GC: removing incomplete staging dir %s", name)
			os.RemoveAll(path)
			continue
		}

		// Remove stale task overlays older than maxAge
		if strings.HasPrefix(name, "task-") {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				log.Printf("overlay GC: removing stale task overlay %s (age=%v)", name, time.Since(info.ModTime()).Round(time.Minute))
				os.RemoveAll(path)
			}
		}
	}
}
