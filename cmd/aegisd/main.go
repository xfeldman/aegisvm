// aegisd is the Aegis daemon — the local control plane for microVM management.
//
// It listens on a unix socket and provides an HTTP API for instance management,
// routing, and secret storage.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xfeldman/aegisvm/internal/api"
	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/image"
	"github.com/xfeldman/aegisvm/internal/lifecycle"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/overlay"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/router"
	"github.com/xfeldman/aegisvm/internal/secrets"
	"github.com/xfeldman/aegisvm/internal/vmm"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg := config.DefaultConfig()
	if err := cfg.EnsureDirs(); err != nil {
		log.Fatalf("create directories: %v", err)
	}

	platform, err := config.DetectPlatform()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("aegisd starting on %s/%s (backend: %s)", platform.OS, platform.Arch, platform.Backend)

	// Initialize VMM backend
	var backend vmm.VMM
	switch platform.Backend {
	case "libkrun":
		backend, err = vmm.NewLibkrunVMM(cfg)
		if err != nil {
			log.Fatalf("init libkrun backend: %v", err)
		}
	case "firecracker":
		log.Fatal("firecracker backend not yet implemented (M4)")
	default:
		log.Fatalf("unknown backend: %s", platform.Backend)
	}

	caps := backend.Capabilities()
	log.Printf("VMM backend: %s (pause=%v)", caps.Name, caps.Pause)

	// Open registry database
	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	log.Printf("registry: %s", cfg.DBPath)

	// Initialize image cache and overlay
	imgCache := image.NewCache(cfg.ImageCacheDir)
	ov := overlay.NewCopyOverlay(cfg.OverlaysDir)

	// Clean up stale overlays from previous crashes
	ov.CleanStale(1 * time.Hour)

	// Create log store
	ls := logstore.NewStore(cfg.LogsDir)

	// Create lifecycle manager (with image cache + overlay for image rootfs prep)
	lm := lifecycle.NewManager(backend, cfg, ls, imgCache, ov)
	lm.OnStateChange(func(id, state string) {
		if err := reg.UpdateState(id, state); err != nil {
			log.Printf("registry state update: %v", err)
		}
	})

	// Initialize secret store
	ss, err := secrets.NewStore(cfg.MasterKeyPath)
	if err != nil {
		log.Fatalf("init secret store: %v", err)
	}
	log.Printf("secret store: %s", cfg.MasterKeyPath)

	// Start router (handle-based routing, no app resolver)
	rtr := router.New(lm, cfg.RouterAddr)
	if err := rtr.Start(); err != nil {
		log.Fatalf("start router: %v", err)
	}

	// Restore instances from registry (they all come back as stopped — VMs are gone)
	if instances, err := reg.ListInstances(); err == nil && len(instances) > 0 {
		restored := 0
		for _, ri := range instances {
			// Build expose ports
			var exposePorts []vmm.PortExpose
			for _, p := range ri.ExposePorts {
				exposePorts = append(exposePorts, vmm.PortExpose{GuestPort: p, Protocol: "http"})
			}

			// Build options
			var opts []lifecycle.InstanceOption
			if ri.Handle != "" {
				opts = append(opts, lifecycle.WithHandle(ri.Handle))
			}
			if ri.ImageRef != "" {
				opts = append(opts, lifecycle.WithImageRef(ri.ImageRef))
			}
			if ri.Workspace != "" {
				opts = append(opts, lifecycle.WithWorkspace(ri.Workspace))
			}
			if len(ri.Env) > 0 {
				opts = append(opts, lifecycle.WithEnv(ri.Env))
			}

			// Re-create in lifecycle manager (state = stopped)
			lm.CreateInstance(ri.ID, ri.Command, exposePorts, opts...)

			// Re-allocate public ports via router (use saved ports for stability)
			for _, guestPort := range ri.ExposePorts {
				requestedPort := 0
				if ri.PublicPorts != nil {
					requestedPort = ri.PublicPorts[guestPort]
				}
				if _, err := rtr.AllocatePort(ri.ID, guestPort, requestedPort, "http"); err != nil {
					// Port may be taken — fall back to random
					if requestedPort > 0 {
						log.Printf("restore port :%d for %s failed, allocating random: %v", requestedPort, ri.ID, err)
						if _, err := rtr.AllocatePort(ri.ID, guestPort, 0, "http"); err != nil {
							log.Printf("restore port for %s: %v", ri.ID, err)
						}
					} else {
						log.Printf("restore port for %s: %v", ri.ID, err)
					}
				}
			}

			restored++
		}
		log.Printf("restored %d instance(s) from registry (all stopped)", restored)
	}

	// Start API server
	server := api.NewServer(cfg, backend, lm, reg, ss, ls, rtr)
	if err := server.Start(); err != nil {
		log.Fatalf("start API server: %v", err)
	}

	// Write PID file
	pidPath := cfg.DataDir + "/aegisd.pid"
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
	defer os.Remove(pidPath)

	log.Printf("aegisd ready (pid %d, socket %s, router %s)", os.Getpid(), cfg.SocketPath, cfg.RouterAddr)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("received %v, shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Stop lifecycle manager (stops all VMs)
	lm.Shutdown()

	if err := rtr.Stop(ctx); err != nil {
		log.Printf("router shutdown: %v", err)
	}

	if err := server.Stop(ctx); err != nil {
		log.Printf("server shutdown: %v", err)
	}

	// Clean up socket
	os.Remove(cfg.SocketPath)

	log.Println("aegisd stopped")
}
