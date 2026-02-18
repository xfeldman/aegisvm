package harness

import (
	"log"
	"os"
	"syscall"
)

// mountWorkspace mounts the "workspace" virtiofs tag at /workspace.
// Behavior depends on whether a workspace was configured by the host:
//   - AEGIS_WORKSPACE=1 set → workspace was configured, mount failure is fatal
//   - AEGIS_WORKSPACE not set → no workspace configured, skip silently
func mountWorkspace() {
	configured := os.Getenv("AEGIS_WORKSPACE") == "1"

	if !configured {
		return
	}

	target := "/workspace"
	_ = os.MkdirAll(target, 0755)
	err := syscall.Mount("workspace", target, "virtiofs", 0, "")
	if err != nil {
		log.Fatalf("workspace mount failed: %v (workspace was configured but virtiofs mount failed)", err)
	}
	log.Printf("workspace mounted at %s", target)
}

// mountEssential mounts /proc and /tmp if they are not already mounted.
func mountEssential() {
	mounts := []struct {
		source string
		target string
		fstype string
		flags  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"tmpfs", "/tmp", "tmpfs", 0},
		{"tmpfs", "/run", "tmpfs", 0},
	}

	for _, m := range mounts {
		_ = os.MkdirAll(m.target, 0755)
		err := syscall.Mount(m.source, m.target, m.fstype, m.flags, "")
		if err != nil && err != syscall.EBUSY {
			log.Printf("mount %s on %s: %v (non-fatal)", m.source, m.target, err)
		}
	}
}
