// aegis-vmm-worker is a helper process that configures and starts a libkrun microVM.
//
// krun_start_enter() takes over the calling process and never returns on success.
// This is why each VM runs in its own worker process, spawned by aegisd.
//
// In gvproxy mode, the worker also embeds the gvisor-tap-vsock networking stack
// in-process. This means SIGSTOP on the worker freezes both the VM and the
// network stack — zero CPU during pause, no separate gvproxy process.
//
// Configuration is passed via the AEGIS_VMM_CONFIG environment variable (JSON).
package main

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lkrun

#include <libkrun.h>
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

type PortForward struct {
	HostPort  int `json:"host_port"`
	GuestPort int `json:"guest_port"`
}

type WorkerConfig struct {
	RootfsPath    string   `json:"rootfs_path"`
	MemoryMB      int      `json:"memory_mb"`
	VCPUs         int      `json:"vcpus"`
	ExecPath      string   `json:"exec_path"`
	HostAddr      string   `json:"host_addr"`       // TSI: host:port | gvproxy: unix socket path for control channel
	PortMap       []string `json:"port_map"`         // e.g. ["8080:80"] — host_port:guest_port (TSI only)
	MappedVolumes []string `json:"mapped_volumes"`   // e.g. ["workspace:/path/to/dir"] — tag:path

	// gvproxy networking (when NetworkMode == "gvproxy")
	NetworkMode string        `json:"network_mode"` // "tsi" (default) or "gvproxy"
	VsockPort   int           `json:"vsock_port"`   // harness control channel vsock port
	ExposePorts []PortForward `json:"expose_ports"` // ports to pre-expose via gvproxy
	SocketDir   string        `json:"socket_dir"`   // directory for network sockets
}

func main() {
	configJSON := os.Getenv("AEGIS_VMM_CONFIG")
	if configJSON == "" {
		fmt.Fprintln(os.Stderr, "AEGIS_VMM_CONFIG not set")
		os.Exit(1)
	}

	var cfg WorkerConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "parse config: %v\n", err)
		os.Exit(1)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "vmm-worker: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg WorkerConfig) error {
	// Set log level to info
	C.krun_set_log_level(3)

	// Create VM context
	ctxID := C.krun_create_ctx()
	if ctxID < 0 {
		return fmt.Errorf("krun_create_ctx failed: %d", ctxID)
	}

	// Configure VM resources
	ret := C.krun_set_vm_config(C.uint32_t(ctxID), C.uint8_t(cfg.VCPUs), C.uint32_t(cfg.MemoryMB))
	if ret < 0 {
		return fmt.Errorf("krun_set_vm_config failed: %d", ret)
	}

	// Set root filesystem (chroot-style directory)
	cRootfs := C.CString(cfg.RootfsPath)
	defer C.free(unsafe.Pointer(cRootfs))
	ret = C.krun_set_root(C.uint32_t(ctxID), cRootfs)
	if ret < 0 {
		return fmt.Errorf("krun_set_root failed: %d", ret)
	}

	// Set the executable (harness binary)
	cExecPath := C.CString(cfg.ExecPath)
	defer C.free(unsafe.Pointer(cExecPath))

	argv := []*C.char{cExecPath, nil}

	// IMPORTANT: We must pass an explicit minimal env, NOT nil.
	// On aarch64, libkrun embeds all env vars into the kernel cmdline,
	// which has a 2048-byte limit. Passing nil inherits the host's full
	// environment and overflows it.
	envVars := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=linux",
	}

	if cfg.NetworkMode == "gvproxy" {
		// --- gvproxy networking: in-process virtio-net via gvisor-tap-vsock ---

		// 1. Initialize the gvisor userspace network stack (NAT + DNS)
		netSockPath, err := startInProcessNetwork(cfg)
		if err != nil {
			return fmt.Errorf("start in-process network: %w", err)
		}

		// 2. Disable implicit vsock+TSI so we can configure our own
		ret = C.krun_disable_implicit_vsock(C.uint32_t(ctxID))
		if ret < 0 {
			return fmt.Errorf("krun_disable_implicit_vsock failed: %d", ret)
		}

		// 3. Add vsock WITHOUT TSI hijacking (tsi_features=0).
		ret = C.krun_add_vsock(C.uint32_t(ctxID), 0)
		if ret < 0 {
			return fmt.Errorf("krun_add_vsock failed: %d", ret)
		}

		// 4. Map vsock port → unix socket for harness control channel.
		cCtlSocket := C.CString(cfg.HostAddr)
		defer C.free(unsafe.Pointer(cCtlSocket))
		ret = C.krun_add_vsock_port(C.uint32_t(ctxID), C.uint32_t(cfg.VsockPort), cCtlSocket)
		if ret < 0 {
			return fmt.Errorf("krun_add_vsock_port failed: %d", ret)
		}

		// 5. Add virtio-net device using the in-process gvproxy socket.
		cNetSocket := C.CString(netSockPath)
		defer C.free(unsafe.Pointer(cNetSocket))
		mac := [6]C.uint8_t{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee}
		ret = C.krun_add_net_unixgram(
			C.uint32_t(ctxID),
			cNetSocket,
			-1,
			&mac[0],
			C.uint32_t(C.COMPAT_NET_FEATURES),
			C.uint32_t(C.NET_FLAG_VFKIT),
		)
		if ret < 0 {
			return fmt.Errorf("krun_add_net_unixgram failed: %d", ret)
		}

		// 6. Tell harness to use vsock + configure eth0
		envVars = append(envVars,
			fmt.Sprintf("AEGIS_VSOCK_PORT=%d", cfg.VsockPort),
			"AEGIS_NET_IP=192.168.127.2/24",
			"AEGIS_NET_GW=192.168.127.1",
		)
	} else {
		// --- TSI networking (legacy): TSI intercepts AF_INET in guest ---
		envVars = append(envVars, fmt.Sprintf("AEGIS_HOST_ADDR=%s", cfg.HostAddr))
	}

	// Signal to the harness whether a workspace volume was configured.
	if len(cfg.MappedVolumes) > 0 {
		envVars = append(envVars, "AEGIS_WORKSPACE=1")
	}
	cEnvPtrs := make([]*C.char, len(envVars)+1)
	for i, e := range envVars {
		cEnvPtrs[i] = C.CString(e)
		defer C.free(unsafe.Pointer(cEnvPtrs[i]))
	}
	cEnvPtrs[len(envVars)] = nil

	ret = C.krun_set_exec(C.uint32_t(ctxID), cExecPath, &argv[0], &cEnvPtrs[0])
	if ret < 0 {
		return fmt.Errorf("krun_set_exec failed: %d", ret)
	}

	// Set port mapping if any ports are exposed (TSI mode only).
	if len(cfg.PortMap) > 0 && cfg.NetworkMode != "gvproxy" {
		cPortPtrs := make([]*C.char, len(cfg.PortMap)+1)
		for i, pm := range cfg.PortMap {
			cPortPtrs[i] = C.CString(pm)
			defer C.free(unsafe.Pointer(cPortPtrs[i]))
		}
		cPortPtrs[len(cfg.PortMap)] = nil
		ret = C.krun_set_port_map(C.uint32_t(ctxID), &cPortPtrs[0])
		if ret < 0 {
			return fmt.Errorf("krun_set_port_map failed: %d", ret)
		}
	}

	// Add virtiofs volumes if any
	for _, vol := range cfg.MappedVolumes {
		parts := splitVolume(vol)
		if len(parts) != 2 {
			return fmt.Errorf("invalid mapped volume format %q, expected tag:path", vol)
		}
		cTag := C.CString(parts[0])
		defer C.free(unsafe.Pointer(cTag))
		cPath := C.CString(parts[1])
		defer C.free(unsafe.Pointer(cPath))
		ret = C.krun_add_virtiofs(C.uint32_t(ctxID), cTag, cPath)
		if ret < 0 {
			return fmt.Errorf("krun_add_virtiofs(%s, %s) failed: %d", parts[0], parts[1], ret)
		}
	}

	// Start the VM — this never returns on success.
	// The calling thread becomes a vCPU. gvproxy goroutines continue
	// on other OS threads. SIGSTOP freezes everything.
	ret = C.krun_start_enter(C.uint32_t(ctxID))

	// Only reached on error
	return fmt.Errorf("krun_start_enter failed: %d", ret)
}

// startInProcessNetwork initializes the gvisor-tap-vsock networking stack
// in-process and pre-exposes any port forwarding rules.
// Returns the path to the unixgram socket for krun_add_net_unixgram.
func startInProcessNetwork(cfg WorkerConfig) (string, error) {
	// 1. Create the gVisor userspace TCP/IP stack with NAT + DNS
	vnCfg := types.Configuration{
		Debug:             false,
		MTU:               1500,
		Subnet:            "192.168.127.0/24",
		GatewayIP:         "192.168.127.1",
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		DNS: []types.Zone{{
			Name:      "dns.internal.",
			DefaultIP: net.ParseIP("192.168.127.2"),
		}},
		Protocol: types.VfkitProtocol,
	}

	vn, err := virtualnetwork.New(&vnCfg)
	if err != nil {
		return "", fmt.Errorf("virtualnetwork.New: %w", err)
	}

	// 2. Create unixgram socket for virtio-net data plane
	netSockPath := filepath.Join(cfg.SocketDir, fmt.Sprintf("net-%d.sock", os.Getpid()))
	os.Remove(netSockPath) // clean stale
	conn, err := transport.ListenUnixgram("unixgram://" + netSockPath)
	if err != nil {
		return "", fmt.Errorf("ListenUnixgram: %w", err)
	}

	// 3. Start the packet forwarding loop in a goroutine.
	// AcceptVfkit blocks until the VM connects, then forwards packets
	// between the unixgram socket and the gVisor stack.
	// This goroutine runs on a separate OS thread from krun_start_enter().
	ctx := context.Background()
	go func() {
		vfkitConn, err := transport.AcceptVfkit(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gvproxy: AcceptVfkit: %v\n", err)
			return
		}
		if err := vn.AcceptVfkit(ctx, vfkitConn); err != nil {
			fmt.Fprintf(os.Stderr, "gvproxy: AcceptVfkit loop: %v\n", err)
		}
	}()

	// 4. Get the ServicesMux for port forwarding (expose/unexpose).
	mux := vn.ServicesMux()

	// 5. Pre-expose port forwarding rules via the ServicesMux (in-process).
	for _, pf := range cfg.ExposePorts {
		if err := exposePort(mux, pf.HostPort, pf.GuestPort); err != nil {
			return "", fmt.Errorf("expose port %d→%d: %w", pf.HostPort, pf.GuestPort, err)
		}
	}

	// 6. Start gvproxy API server on a unix socket for runtime port management.
	// aegisd calls this to expose/unexpose ports dynamically.
	apiSockPath := filepath.Join(cfg.SocketDir, fmt.Sprintf("gvproxy-%d.sock", os.Getpid()))
	os.Remove(apiSockPath)
	apiLn, err := net.Listen("unix", apiSockPath)
	if err != nil {
		return "", fmt.Errorf("gvproxy API listen: %w", err)
	}
	go http.Serve(apiLn, mux)

	return netSockPath, nil
}

// exposePort calls the gvproxy forwarder API in-process via httptest.
func exposePort(mux http.Handler, hostPort, guestPort int) error {
	body, err := json.Marshal(types.ExposeRequest{
		Local:    fmt.Sprintf("127.0.0.1:%d", hostPort),
		Remote:   fmt.Sprintf("192.168.127.2:%d", guestPort),
		Protocol: types.TCP,
	})
	if err != nil {
		return err
	}

	req := httptest.NewRequest("POST", "/services/forwarder/expose", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code >= 300 {
		return fmt.Errorf("expose returned %d: %s", rec.Code, rec.Body.String())
	}
	return nil
}

// splitVolume splits "tag:path" into [tag, path], handling paths with colons.
func splitVolume(s string) []string {
	idx := 0
	for i, c := range s {
		if c == ':' {
			idx = i
			break
		}
	}
	if idx == 0 {
		return nil
	}
	return []string{s[:idx], s[idx+1:]}
}
