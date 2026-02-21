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

// mountEssential sets up the guest filesystem:
//  1. Mount /proc, writable tmpfs on /tmp and /run
//  2. Remount / read-only to preserve release rootfs immutability
//
// libkrun's krun_set_root() exposes the host directory via virtiofs read-write.
// Without the read-only remount, any guest write to /usr, /etc, etc. would
// mutate the release directory on the host, breaking immutability.
func mountEssential() {
	// Phase 1: Mount writable filesystems first (before making / read-only)
	writableMounts := []struct {
		source string
		target string
		fstype string
	}{
		{"proc", "/proc", "proc"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"tmpfs", "/run", "tmpfs"},
		{"tmpfs", "/var", "tmpfs"},
	}

	for _, m := range writableMounts {
		_ = os.MkdirAll(m.target, 0755)
		err := syscall.Mount(m.source, m.target, m.fstype, 0, "")
		if err != nil && err != syscall.EBUSY {
			log.Printf("mount %s on %s: %v (non-fatal)", m.source, m.target, err)
		}
	}

	// Ensure /etc/resolv.conf exists (OCI images often lack it).
	// Must happen before read-only remount since /etc lives on the rootfs.
	if _, err := os.Stat("/etc/resolv.conf"); os.IsNotExist(err) {
		if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
			log.Printf("write /etc/resolv.conf: %v (non-fatal, DNS may not work)", err)
		}
	}

	// Ensure /etc/hosts exists (OCI images often lack it).
	// Many apps and runtimes expect localhost to resolve via /etc/hosts.
	if _, err := os.Stat("/etc/hosts"); os.IsNotExist(err) {
		hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost\n"
		if err := os.WriteFile("/etc/hosts", []byte(hosts), 0644); err != nil {
			log.Printf("write /etc/hosts: %v (non-fatal)", err)
		}
	}

	// Phase 2: Remount / read-only to protect the release rootfs.
	// MS_REMOUNT | MS_RDONLY changes an existing mount to read-only.
	// This only affects the root virtiofs — /tmp, /run, /var, /workspace
	// are separate mounts and remain writable.
	err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
	if err != nil {
		log.Printf("remount / read-only: %v (non-fatal, rootfs writes will not be blocked)", err)
	} else {
		log.Println("rootfs remounted read-only")
	}
}
