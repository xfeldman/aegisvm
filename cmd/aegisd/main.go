// aegisd is the Aegis daemon — the local control plane for microVM management.
//
// It listens on a unix socket and provides an HTTP API for instance management,
// routing, and secret storage.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xfeldman/aegisvm/internal/api"
	"github.com/xfeldman/aegisvm/internal/config"
	"github.com/xfeldman/aegisvm/internal/daemon"
	"github.com/xfeldman/aegisvm/internal/image"
	"github.com/xfeldman/aegisvm/internal/lifecycle"
	"github.com/xfeldman/aegisvm/internal/logstore"
	"github.com/xfeldman/aegisvm/internal/overlay"
	"github.com/xfeldman/aegisvm/internal/registry"
	"github.com/xfeldman/aegisvm/internal/router"
	"github.com/xfeldman/aegisvm/internal/secrets"
	"github.com/xfeldman/aegisvm/internal/tether"
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

	// Resolve network backend (auto → gvproxy on darwin, tsi elsewhere)
	cfg.ResolveNetworkBackend()
	log.Printf("network backend: %s", cfg.NetworkBackend)

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
	log.Printf("VMM backend: %s (pause=%v, network=%s)", caps.Name, caps.Pause, caps.NetworkBackend)

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
		if state == "stopped" {
			reg.UpdateStoppedAt(id, time.Now())
		} else {
			reg.UpdateStoppedAt(id, time.Time{})
		}
	})

	// Initialize secret store
	ss, err := secrets.NewStore(cfg.MasterKeyPath)
	if err != nil {
		log.Fatalf("init secret store: %v", err)
	}
	log.Printf("secret store: %s", cfg.MasterKeyPath)

	// Pass secret store, registry, and tether store to lifecycle manager
	lm.SetSecretStore(ss)
	lm.SetRegistry(reg)
	lm.SetTetherStore(tether.NewStore())

	// Start router (handle-based routing, no app resolver)
	rtr := router.New(lm, cfg.RouterAddr)
	if err := rtr.Start(); err != nil {
		log.Fatalf("start router: %v", err)
	}

	// Public port lookup — used by guest API responses
	lm.SetPublicPortsLookup(func(id string) map[int]int {
		ports := make(map[int]int)
		for _, ep := range rtr.GetAllPublicPorts(id) {
			ports[ep.GuestPort] = ep.PublicPort
		}
		return ports
	})

	// Router port allocation — called from CreateInstance for every new instance
	lm.OnAllocatePorts(func(inst *lifecycle.Instance) {
		for _, ep := range inst.ExposePorts {
			if _, err := rtr.AllocatePort(inst.ID, ep.GuestPort, 0, ep.Protocol); err != nil {
				log.Printf("allocate port for %s guest:%d: %v", inst.ID, ep.GuestPort, err)
			}
		}
	})

	// Create daemon manager for per-instance sidecar processes (e.g., gateways)
	dm := daemon.NewManager(cfg.BinDir, cfg.DataDir, cfg.SocketPath)

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
			opts = append(opts, lifecycle.WithEnabled(ri.Enabled))
			if ri.MemoryMB > 0 {
				opts = append(opts, lifecycle.WithMemory(ri.MemoryMB))
			}
			if ri.VCPUs > 0 {
				opts = append(opts, lifecycle.WithVCPUs(ri.VCPUs))
			}
			if ri.ParentID != "" {
				opts = append(opts, lifecycle.WithParentID(ri.ParentID))
			}
			if ri.Capabilities != "" {
				var caps lifecycle.CapabilityToken
				if json.Unmarshal([]byte(ri.Capabilities), &caps) == nil {
					opts = append(opts, lifecycle.WithCapabilities(&caps))
				}
			}
			if ri.Kit != "" {
				opts = append(opts, lifecycle.WithKit(ri.Kit))
			}

			// Re-create in lifecycle manager (state = stopped)
			inst := lm.CreateInstance(ri.ID, ri.Command, exposePorts, opts...)

			// Restore stopped_at from registry
			if !ri.StoppedAt.IsZero() {
				inst.StoppedAt = ri.StoppedAt
			}

			// Re-allocate public ports via router only if enabled
			// Disabled instances are unreachable — no listeners allocated
			if ri.Enabled {
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
			}

			// Start instance daemons for enabled kit instances
			if ri.Enabled && ri.Kit != "" {
				handle := ri.Handle
				if handle == "" {
					handle = ri.ID
				}
				if err := dm.StartDaemons(ri.ID, handle, ri.Kit); err != nil {
					log.Printf("start daemons for %s: %v", ri.ID, err)
				}
			}

			restored++
		}
		log.Printf("restored %d instance(s) from registry (all stopped)", restored)
	}

	// Start API server
	server := api.NewServer(cfg, backend, lm, reg, ss, ls, rtr, dm)
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

	// Stop all instance daemons (gateways, etc.)
	dm.StopAll()

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
