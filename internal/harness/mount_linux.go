package harness

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// parseCmdlineEnv reads /proc/cmdline and sets environment variables from KEY=VALUE
// tokens. Only sets vars that are not already in the environment. Some backends
// pass env vars via kernel cmdline rather than inheriting them from the parent
// process — this ensures AEGIS_* vars are available regardless of boot method.
func parseCmdlineEnv() {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return // /proc not mounted yet or not available
	}

	// Kernel cmdline env vars we care about
	envPrefixes := []string{"AEGIS_", "PATH=", "HOME=", "TERM="}

	for _, token := range strings.Fields(string(data)) {
		eqIdx := strings.IndexByte(token, '=')
		if eqIdx < 1 {
			continue
		}
		key := token[:eqIdx]
		value := token[eqIdx+1:]

		// Only set vars with recognized prefixes
		match := false
		for _, prefix := range envPrefixes {
			if strings.HasPrefix(token, prefix) {
				match = true
				break
			}
		}
		if !match {
			continue
		}

		// Don't overwrite existing env vars
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		os.Setenv(key, value)
		log.Printf("cmdline env: %s=%s", key, value)
	}
}

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
	// Parse kernel cmdline for AEGIS_* env vars and standard env (PATH, HOME, TERM).
	// Safety net for backends that pass env via kernel cmdline rather than process env.
	parseCmdlineEnv()

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

	// Configure network if AEGIS_NET_IP is set (gvproxy mode).
	// Must happen before /etc/resolv.conf setup since it may override DNS.
	// Must happen before read-only remount since it writes to /etc.
	setupNetwork()

	// Ensure /etc/resolv.conf exists (OCI images often lack it).
	// In gvproxy mode, setupNetwork() already wrote it with the gateway DNS.
	// In TSI mode, fall back to 8.8.8.8.
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

// setupNetwork configures eth0 when AEGIS_NET_IP is set (gvproxy mode).
// In TSI mode (no AEGIS_NET_IP), this is a no-op.
//
// Uses netlink syscalls directly — no dependency on iproute2 or busybox in the
// guest rootfs. This ensures networking works with any OCI image (Debian-slim,
// Alpine, distroless, etc.).
func setupNetwork() {
	ip := os.Getenv("AEGIS_NET_IP")
	gw := os.Getenv("AEGIS_NET_GW")
	if ip == "" {
		return // TSI mode, no network setup needed
	}

	dns := os.Getenv("AEGIS_NET_DNS")
	if dns == "" {
		dns = gw // default: gateway runs DNS (gvproxy built-in)
	}

	// Wait for eth0 to appear (virtio-net device may take a moment)
	if err := waitForInterface("eth0", 5*time.Second); err != nil {
		log.Printf("setupNetwork: %v (network may not work)", err)
		return
	}

	// Get eth0 interface index
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		log.Printf("setupNetwork: get eth0: %v (network may not work)", err)
		return
	}

	// 1. Bring interface up
	if err := netlinkSetLinkUp(iface.Index); err != nil {
		log.Printf("setupNetwork link up: %v", err)
		return
	}

	// 2. Add IP address
	if err := netlinkAddAddr(iface.Index, ip); err != nil {
		log.Printf("setupNetwork add addr: %v", err)
		return
	}

	// 3. Add default route
	if err := netlinkAddDefaultRoute(gw); err != nil {
		log.Printf("setupNetwork add route: %v", err)
		return
	}

	// Write resolv.conf with gvproxy's DNS server (overwrites any existing one).
	// Remove existing first to handle both regular file and symlink cases.
	os.Remove("/etc/resolv.conf")
	if err := os.WriteFile("/etc/resolv.conf", []byte(fmt.Sprintf("nameserver %s\n", dns)), 0644); err != nil {
		log.Printf("setupNetwork write resolv.conf: %v", err)
	}

	log.Printf("network configured: %s via %s (dns %s)", ip, gw, dns)
}

// waitForInterface polls /sys/class/net/{name} until the interface appears.
func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/sys/class/net/%s", name)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("interface %s did not appear within %v", name, timeout)
}
