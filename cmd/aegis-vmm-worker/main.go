// aegis-vmm-worker is a helper process that configures and starts a libkrun microVM.
//
// krun_start_enter() takes over the calling process and never returns on success.
// This is why each VM runs in its own worker process, spawned by aegisd.
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
	"encoding/json"
	"fmt"
	"os"
	"unsafe"
)

type WorkerConfig struct {
	RootfsPath string `json:"rootfs_path"`
	MemoryMB   int    `json:"memory_mb"`
	VCPUs      int    `json:"vcpus"`
	ExecPath   string `json:"exec_path"`
	HostAddr   string `json:"host_addr"` // host:port for harness to connect back to
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

	// Note: we intentionally keep the implicit console enabled for future
	// remote console support (aegis logs, aegis attach). The "Failed to set
	// terminal to raw mode" warning when stdout isn't a TTY is harmless.

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
	//
	// AEGIS_HOST_ADDR tells the harness where to connect back to the host.
	// TSI (Transparent Socket Impersonation) intercepts AF_INET connections
	// in the guest and routes them through vsock to the host, so 127.0.0.1:PORT
	// from inside the VM reaches the host's actual localhost.
	envVars := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=linux",
		fmt.Sprintf("AEGIS_HOST_ADDR=%s", cfg.HostAddr),
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

	// Start the VM — this never returns on success.
	// The process becomes the VM. On guest exit, the process exits.
	ret = C.krun_start_enter(C.uint32_t(ctxID))

	// Only reached on error
	return fmt.Errorf("krun_start_enter failed: %d", ret)
}
