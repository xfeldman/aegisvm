package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/xfeldman/aegis/internal/registry"
)

// Secret API request/response types

type setSecretRequest struct {
	Value string `json:"value"`
}

type secretResponse struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
}

func secretToResponse(s *registry.Secret) secretResponse {
	return secretResponse{
		Name:      s.Name,
		Scope:     s.Scope,
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
	}
}

// handleSetSecret sets (or updates) an app-scoped secret.
// PUT /v1/apps/{id}/secrets/{name}
func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	appIDOrName := pathParam(r, "id")
	name := pathParam(r, "name")

	app, err := s.resolveApp(appIDOrName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	var req setSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}

	encrypted, err := s.secretStore.EncryptString(req.Value)
	if err != nil {
		log.Printf("encrypt secret: %v", err)
		writeError(w, http.StatusInternalServerError, "encryption failed")
		return
	}

	secret := &registry.Secret{
		ID:             fmt.Sprintf("sec-%d", time.Now().UnixNano()),
		AppID:          app.ID,
		Name:           name,
		EncryptedValue: encrypted,
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	}

	if err := s.registry.SaveSecret(secret); err != nil {
		log.Printf("save secret: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save secret")
		return
	}

	log.Printf("secret set: %s/%s", app.Name, name)
	writeJSON(w, http.StatusOK, secretToResponse(secret))
}

// handleListSecrets lists secrets for an app (names only, no values).
// GET /v1/apps/{id}/secrets
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	appIDOrName := pathParam(r, "id")

	app, err := s.resolveApp(appIDOrName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	secrets, err := s.registry.ListSecrets(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list secrets: %v", err))
		return
	}

	resp := make([]secretResponse, 0, len(secrets))
	for _, sec := range secrets {
		resp = append(resp, secretToResponse(sec))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteSecret deletes an app-scoped secret.
// DELETE /v1/apps/{id}/secrets/{name}
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	appIDOrName := pathParam(r, "id")
	name := pathParam(r, "name")

	app, err := s.resolveApp(appIDOrName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	if err := s.registry.DeleteSecretByName(app.ID, name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	log.Printf("secret deleted: %s/%s", app.Name, name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleSetWorkspaceSecret sets (or updates) a workspace-scoped secret.
// PUT /v1/secrets/{name}
func (s *Server) handleSetWorkspaceSecret(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")

	var req setSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}

	encrypted, err := s.secretStore.EncryptString(req.Value)
	if err != nil {
		log.Printf("encrypt secret: %v", err)
		writeError(w, http.StatusInternalServerError, "encryption failed")
		return
	}

	secret := &registry.Secret{
		ID:             fmt.Sprintf("sec-%d", time.Now().UnixNano()),
		AppID:          "",
		Name:           name,
		EncryptedValue: encrypted,
		Scope:          "per_workspace",
		CreatedAt:      time.Now(),
	}

	if err := s.registry.SaveSecret(secret); err != nil {
		log.Printf("save workspace secret: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save secret")
		return
	}

	log.Printf("workspace secret set: %s", name)
	writeJSON(w, http.StatusOK, secretToResponse(secret))
}

// handleListWorkspaceSecrets lists workspace-scoped secrets (names only).
// GET /v1/secrets
func (s *Server) handleListWorkspaceSecrets(w http.ResponseWriter, r *http.Request) {
	secrets, err := s.registry.ListWorkspaceSecrets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list secrets: %v", err))
		return
	}

	resp := make([]secretResponse, 0, len(secrets))
	for _, sec := range secrets {
		resp = append(resp, secretToResponse(sec))
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveSecrets decrypts app-scoped + workspace-scoped secrets into a merged env map.
// App-scoped secrets take precedence over workspace-scoped ones.
func (s *Server) resolveSecrets(appID string) (map[string]string, error) {
	env := make(map[string]string)

	// Load workspace secrets first (lower priority)
	wsSecrets, err := s.registry.ListWorkspaceSecrets()
	if err != nil {
		return nil, fmt.Errorf("list workspace secrets: %w", err)
	}
	for _, sec := range wsSecrets {
		val, err := s.secretStore.DecryptString(sec.EncryptedValue)
		if err != nil {
			return nil, fmt.Errorf("decrypt workspace secret %s: %w", sec.Name, err)
		}
		env[sec.Name] = val
	}

	// Load app secrets (higher priority, overwrites workspace)
	if appID != "" {
		appSecrets, err := s.registry.ListSecrets(appID)
		if err != nil {
			return nil, fmt.Errorf("list app secrets: %w", err)
		}
		for _, sec := range appSecrets {
			val, err := s.secretStore.DecryptString(sec.EncryptedValue)
			if err != nil {
				return nil, fmt.Errorf("decrypt app secret %s: %w", sec.Name, err)
			}
			env[sec.Name] = val
		}
	}

	return env, nil
}
