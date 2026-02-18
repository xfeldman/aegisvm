// Package overlay provides rootfs copy management for releases.
// Each release gets its own full copy of the base rootfs so that
// harness injection and runtime modifications are isolated.
package overlay

import "context"

// Overlay manages rootfs copies for releases.
type Overlay interface {
	// Create copies sourceDir into a new directory identified by destID.
	// Returns the path to the created directory.
	Create(ctx context.Context, sourceDir, destID string) (string, error)

	// Remove deletes the directory for the given ID.
	Remove(id string) error

	// Path returns the directory path for the given ID.
	Path(id string) string
}
