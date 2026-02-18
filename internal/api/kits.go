package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/xfeldman/aegis/internal/registry"
)

// Kit API request/response types

type registerKitRequest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	Config      registry.KitConfig `json:"config"`
	ImageRef    string            `json:"image_ref"`
}

// handleRegisterKit registers a new kit.
// POST /v1/kits
func (s *Server) handleRegisterKit(w http.ResponseWriter, r *http.Request) {
	var req registerKitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Version == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}
	if req.ImageRef == "" {
		writeError(w, http.StatusBadRequest, "image_ref is required")
		return
	}

	kit := &registry.Kit{
		Name:        req.Name,
		Version:     req.Version,
		Description: req.Description,
		Config:      req.Config,
		ImageRef:    req.ImageRef,
		InstalledAt: time.Now(),
	}

	if err := s.registry.SaveKit(kit); err != nil {
		log.Printf("save kit: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save kit")
		return
	}

	log.Printf("kit registered: %s v%s", kit.Name, kit.Version)
	writeJSON(w, http.StatusCreated, kit)
}

// handleListKits returns all installed kits.
// GET /v1/kits
func (s *Server) handleListKits(w http.ResponseWriter, r *http.Request) {
	kits, err := s.registry.ListKits()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list kits: %v", err))
		return
	}
	if kits == nil {
		kits = []*registry.Kit{}
	}
	writeJSON(w, http.StatusOK, kits)
}

// handleGetKit returns a kit by name.
// GET /v1/kits/{name}
func (s *Server) handleGetKit(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")

	kit, err := s.registry.GetKit(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if kit == nil {
		writeError(w, http.StatusNotFound, "kit not found")
		return
	}
	writeJSON(w, http.StatusOK, kit)
}

// handleDeleteKit removes a kit.
// DELETE /v1/kits/{name}
func (s *Server) handleDeleteKit(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")

	if err := s.registry.DeleteKit(name); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "kit not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("kit deleted: %s", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
