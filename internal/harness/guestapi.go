package harness

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

const guestAPIAddr = "127.0.0.1:7777"

// startGuestAPIServer starts the guest-facing HTTP API on localhost:7777.
// Guest processes call this to spawn/manage child instances via the host.
// No authentication required — the harness attaches the capability token automatically.
func startGuestAPIServer(hrpc *harnessRPC) {
	mux := http.NewServeMux()

	// Instance management
	mux.HandleFunc("POST /v1/instances", func(w http.ResponseWriter, r *http.Request) {
		handleGuestSpawn(w, r, hrpc)
	})
	mux.HandleFunc("GET /v1/instances", func(w http.ResponseWriter, r *http.Request) {
		handleGuestListChildren(w, r, hrpc)
	})
	mux.HandleFunc("POST /v1/instances/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		handleGuestStopChild(w, r, hrpc)
	})

	// Self
	mux.HandleFunc("GET /v1/self", func(w http.ResponseWriter, r *http.Request) {
		handleGuestSelfInfo(w, r, hrpc)
	})
	mux.HandleFunc("POST /v1/self/keepalive", func(w http.ResponseWriter, r *http.Request) {
		handleGuestKeepalive(w, r, hrpc)
	})
	mux.HandleFunc("DELETE /v1/self/keepalive", func(w http.ResponseWriter, r *http.Request) {
		handleGuestKeepaliveRelease(w, r, hrpc)
	})

	log.Printf("guest API listening on %s", guestAPIAddr)
	if err := http.ListenAndServe(guestAPIAddr, mux); err != nil {
		log.Printf("guest API server: %v", err)
	}
}

// handleGuestSpawn spawns a child instance via the host.
func handleGuestSpawn(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	var req map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGuestError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := hrpc.Call("guest.spawn", req)
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleGuestListChildren lists child instances of this parent.
func handleGuestListChildren(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	result, err := hrpc.Call("guest.list_children", nil)
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleGuestStopChild stops a child instance.
func handleGuestStopChild(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	childID := r.PathValue("id")
	if childID == "" {
		writeGuestError(w, http.StatusBadRequest, "missing instance id")
		return
	}

	result, err := hrpc.Call("guest.stop_child", map[string]string{"child_id": childID})
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleGuestSelfInfo returns info about this instance.
func handleGuestSelfInfo(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	result, err := hrpc.Call("guest.self_info", nil)
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleGuestKeepalive acquires a keepalive lease.
func handleGuestKeepalive(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	var req map[string]interface{}
	json.NewDecoder(r.Body).Decode(&req)
	if req == nil {
		req = map[string]interface{}{"ttl_ms": 30000, "reason": "guest"}
	}

	// Keepalive uses the existing notification path (not a call)
	sendNotification(hrpc.conn, "keepalive", req)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleGuestKeepaliveRelease releases the keepalive lease.
func handleGuestKeepaliveRelease(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	sendNotification(hrpc.conn, "keepalive.release", nil)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

func writeGuestError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": message},
	})
}

// injectGuestAPIEnv adds AEGIS_GUEST_API and AEGIS_INSTANCE_ID to the environment
// if the guest API is available (capability token received).
func injectGuestAPIEnv(env []string, hrpc *harnessRPC) []string {
	env = append(env, "AEGIS_GUEST_API=http://"+guestAPIAddr)
	instanceID := os.Getenv("AEGIS_INSTANCE_ID")
	if instanceID == "" {
		instanceID = "unknown"
	}
	env = append(env, "AEGIS_INSTANCE_ID="+instanceID)
	return env
}

// pathParam extracts a path parameter from Go 1.22+ ServeMux patterns.
func pathParam(r *http.Request, name string) string {
	// Go 1.22 ServeMux uses PathValue
	val := r.PathValue(name)
	if val != "" {
		return val
	}
	// Fallback: extract from URL path
	parts := strings.Split(r.URL.Path, "/")
	for i, p := range parts {
		if p == name && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
