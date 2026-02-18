package harness

import (
	"log"
	"os"
	"syscall"
)

// mountWorkspace attempts to mount the "workspace" virtiofs tag at /workspace.
// This is best-effort — if the tag isn't available (no workspace configured),
// the mount will fail silently.
func mountWorkspace() {
	target := "/workspace"
	_ = os.MkdirAll(target, 0755)
	err := syscall.Mount("workspace", target, "virtiofs", 0, "")
	if err != nil {
		log.Printf("workspace mount: %v (non-fatal, workspace may not be configured)", err)
	} else {
		log.Printf("workspace mounted at %s", target)
	}
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
