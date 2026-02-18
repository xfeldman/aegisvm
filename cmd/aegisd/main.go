// aegisd is the Aegis daemon — the local control plane for microVM management.
//
// It listens on a unix socket and provides an HTTP API for task execution,
// instance management, and (in later milestones) routing and kit integration.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xfeldman/aegis/internal/api"
	"github.com/xfeldman/aegis/internal/config"
	"github.com/xfeldman/aegis/internal/image"
	"github.com/xfeldman/aegis/internal/lifecycle"
	"github.com/xfeldman/aegis/internal/overlay"
	"github.com/xfeldman/aegis/internal/registry"
	"github.com/xfeldman/aegis/internal/router"
	"github.com/xfeldman/aegis/internal/secrets"
	"github.com/xfeldman/aegis/internal/vmm"
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
	log.Printf("VMM backend: %s (pause=%v, snapshot=%v)", caps.Name, caps.Pause, caps.SnapshotRestore)

	// Open registry database
	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	log.Printf("registry: %s", cfg.DBPath)

	// Initialize image cache and overlay
	imgCache := image.NewCache(cfg.ImageCacheDir)
	ov := overlay.NewCopyOverlay(cfg.ReleasesDir)

	// Clean up stale task overlays and incomplete staging dirs from previous crashes
	ov.CleanStale(1 * time.Hour)

	// Create lifecycle manager
	lm := lifecycle.NewManager(backend, cfg)
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

	// Start router with app resolver
	rtr := router.New(lm, cfg.RouterAddr, &registryAppResolver{db: reg})
	if err := rtr.Start(); err != nil {
		log.Fatalf("start router: %v", err)
	}

	// Start API server
	server := api.NewServer(cfg, backend, lm, reg, imgCache, ov, ss)
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

// registryAppResolver adapts the registry DB to the router.AppResolver interface.
type registryAppResolver struct {
	db *registry.DB
}

func (r *registryAppResolver) GetAppByName(name string) (string, bool) {
	app, err := r.db.GetAppByName(name)
	if err != nil || app == nil {
		return "", false
	}
	return app.ID, true
}
