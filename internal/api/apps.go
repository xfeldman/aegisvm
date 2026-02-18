package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xfeldman/aegis/internal/image"
	"github.com/xfeldman/aegis/internal/lifecycle"
	"github.com/xfeldman/aegis/internal/registry"
	"github.com/xfeldman/aegis/internal/vmm"
)

// App API request/response types

type createAppRequest struct {
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Command     []string `json:"command"`
	ExposePorts []int    `json:"expose_ports"`
}

type publishRequest struct {
	Label string `json:"label,omitempty"`
}

type appResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Command     []string `json:"command"`
	ExposePorts []int    `json:"expose_ports"`
	CreatedAt   string   `json:"created_at"`
}

func appToResponse(app *registry.App) appResponse {
	return appResponse{
		ID:          app.ID,
		Name:        app.Name,
		Image:       app.Image,
		Command:     app.Command,
		ExposePorts: app.ExposePorts,
		CreatedAt:   app.CreatedAt.Format(time.RFC3339),
	}
}

// handleCreateApp creates a new app.
func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if !isValidID(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid app name (alphanumeric, hyphens, underscores only)")
		return
	}

	// Check for duplicate name
	existing, _ := s.registry.GetAppByName(req.Name)
	if existing != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("app %q already exists", req.Name))
		return
	}

	app := &registry.App{
		ID:          fmt.Sprintf("app-%d", time.Now().UnixNano()),
		Name:        req.Name,
		Image:       req.Image,
		Command:     req.Command,
		ExposePorts: req.ExposePorts,
		CreatedAt:   time.Now(),
	}

	if err := s.registry.SaveApp(app); err != nil {
		log.Printf("save app: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save app")
		return
	}

	log.Printf("app created: %s (%s)", app.Name, app.ID)
	writeJSON(w, http.StatusCreated, appToResponse(app))
}

// handleListApps returns all apps.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.registry.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list apps: %v", err))
		return
	}

	resp := make([]appResponse, 0, len(apps))
	for _, app := range apps {
		resp = append(resp, appToResponse(app))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetApp returns an app by ID or name.
func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	app, err := s.resolveApp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	writeJSON(w, http.StatusOK, appToResponse(app))
}

// handleDeleteApp deletes an app and its releases.
func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	app, err := s.resolveApp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	// Stop any running instances for this app
	if inst := s.lifecycle.GetInstanceByApp(app.ID); inst != nil {
		s.lifecycle.StopInstance(inst.ID)
	}

	// Clean up release rootfs directories
	releases, _ := s.registry.ListReleases(app.ID)
	for _, rel := range releases {
		os.RemoveAll(rel.RootfsPath)
	}

	if err := s.registry.DeleteApp(app.ID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete app: %v", err))
		return
	}

	log.Printf("app deleted: %s (%s)", app.Name, app.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handlePublishApp publishes a new release for an app.
func (s *Server) handlePublishApp(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	app, err := s.resolveApp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	var req publishRequest
	json.NewDecoder(r.Body).Decode(&req) // optional body

	if s.imageCache == nil {
		writeError(w, http.StatusInternalServerError, "image cache not initialized")
		return
	}

	// 1. Pull (or use cached) image
	cachedDir, digest, err := s.imageCache.GetOrPull(r.Context(), app.Image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("pull image: %v", err))
		return
	}

	// 2. Create release rootfs copy
	releaseID := fmt.Sprintf("rel-%d", time.Now().UnixNano())
	releaseDir, err := s.overlay.Create(r.Context(), cachedDir, releaseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("create overlay: %v", err))
		return
	}

	// 3. Inject harness into release rootfs
	harnessBin := filepath.Join(s.cfg.BinDir, "aegis-harness")
	if err := image.InjectHarness(releaseDir, harnessBin); err != nil {
		s.overlay.Remove(releaseID)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("inject harness: %v", err))
		return
	}

	// 4. Record release in registry
	release := &registry.Release{
		ID:          releaseID,
		AppID:       app.ID,
		ImageDigest: digest,
		RootfsPath:  releaseDir,
		Label:       req.Label,
		CreatedAt:   time.Now(),
	}
	if err := s.registry.SaveRelease(release); err != nil {
		s.overlay.Remove(releaseID)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save release: %v", err))
		return
	}

	log.Printf("release published: %s (app=%s, digest=%s)", releaseID, app.Name, digest)
	writeJSON(w, http.StatusCreated, release)
}

// handleListReleases returns all releases for an app.
func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	app, err := s.resolveApp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	releases, err := s.registry.ListReleases(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list releases: %v", err))
		return
	}
	if releases == nil {
		releases = []*registry.Release{}
	}
	writeJSON(w, http.StatusOK, releases)
}

// handleServeApp starts serving an app from its latest release.
func (s *Server) handleServeApp(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	app, err := s.resolveApp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	// Check if already serving
	if inst := s.lifecycle.GetInstanceByApp(app.ID); inst != nil {
		writeJSON(w, http.StatusOK, instanceResponse{
			ID:          inst.ID,
			State:       inst.State,
			Command:     app.Command,
			ExposePorts: app.ExposePorts,
			RouterAddr:  s.cfg.RouterAddr,
		})
		return
	}

	// Get latest release
	release, err := s.registry.GetLatestRelease(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get release: %v", err))
		return
	}
	if release == nil {
		writeError(w, http.StatusBadRequest, "no releases found — publish first")
		return
	}

	// Build PortExpose list
	var exposePorts []vmm.PortExpose
	for _, p := range app.ExposePorts {
		exposePorts = append(exposePorts, vmm.PortExpose{
			GuestPort: p,
			Protocol:  "http",
		})
	}

	// Create workspace directory
	workspacePath := filepath.Join(s.cfg.WorkspacesDir, app.ID)
	os.MkdirAll(workspacePath, 0755)

	// Create instance with release rootfs + workspace
	instID := fmt.Sprintf("inst-%d", time.Now().UnixNano())
	s.lifecycle.CreateInstance(instID, app.Command, exposePorts,
		lifecycle.WithApp(app.ID, release.ID),
		lifecycle.WithRootfs(release.RootfsPath),
		lifecycle.WithWorkspace(workspacePath),
	)

	// Persist to registry
	if s.registry != nil {
		regInst := &registry.Instance{
			ID:          instID,
			State:       "stopped",
			Command:     app.Command,
			ExposePorts: app.ExposePorts,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := s.registry.SaveInstance(regInst); err != nil {
			log.Printf("save instance to registry: %v", err)
		}
	}

	// Boot the instance
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.lifecycle.EnsureInstance(ctx, instID); err != nil {
			log.Printf("instance %s boot failed: %v", instID, err)
		}
	}()

	log.Printf("serving app %s (instance=%s, release=%s)", app.Name, instID, release.ID)
	writeJSON(w, http.StatusCreated, instanceResponse{
		ID:          instID,
		State:       "starting",
		Command:     app.Command,
		ExposePorts: app.ExposePorts,
		RouterAddr:  s.cfg.RouterAddr,
	})
}

// resolveApp looks up an app by ID or name.
func (s *Server) resolveApp(idOrName string) (*registry.App, error) {
	// Try by ID first
	app, err := s.registry.GetApp(idOrName)
	if err != nil {
		return nil, err
	}
	if app != nil {
		return app, nil
	}
	// Try by name
	return s.registry.GetAppByName(idOrName)
}
