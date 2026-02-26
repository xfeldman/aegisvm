package harness

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"syscall"
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

	// Tether egress (agent runtime → host)
	mux.HandleFunc("POST /v1/tether/send", func(w http.ResponseWriter, r *http.Request) {
		handleGuestTetherSend(w, r, hrpc)
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

	// Self-restart (agent config reload)
	mux.HandleFunc("POST /v1/self/restart", func(w http.ResponseWriter, r *http.Request) {
		handleGuestRestart(w, r, hrpc)
	})

	// Runtime port expose/unexpose
	mux.HandleFunc("POST /v1/self/expose", func(w http.ResponseWriter, r *http.Request) {
		handleGuestExpose(w, r, hrpc)
	})
	mux.HandleFunc("DELETE /v1/self/expose/{guest_port}", func(w http.ResponseWriter, r *http.Request) {
		handleGuestUnexpose(w, r, hrpc)
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

// handleGuestTetherSend forwards a tether frame from the agent runtime to aegisd.
func handleGuestTetherSend(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	var frame json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
		writeGuestError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Send as a tether.frame notification to aegisd (no response expected)
	if err := sendNotification(hrpc.conn, "tether.frame", frame); err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleGuestRestart sends SIGTERM to the primary process, triggering a restart.
// The restart is handled by the exit goroutine in startPrimaryProcess.
func handleGuestRestart(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	tracker := hrpc.tracker
	tracker.mu.Lock()
	primary := tracker.primary
	if primary == nil || primary.Process == nil {
		tracker.mu.Unlock()
		writeGuestError(w, http.StatusBadRequest, "no primary process to restart")
		return
	}
	tracker.restartRequested = true
	tracker.mu.Unlock()

	log.Println("self_restart: sending SIGTERM to primary process")
	primary.Process.Signal(syscall.SIGTERM)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"message":"restart initiated"}`)
}

// handleGuestExpose exposes a port via the host, then starts a local port proxy.
func handleGuestExpose(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	var req struct {
		GuestPort  int    `json:"guest_port"`
		PublicPort int    `json:"public_port"`
		Protocol   string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGuestError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.GuestPort <= 0 {
		writeGuestError(w, http.StatusBadRequest, "guest_port is required")
		return
	}

	result, err := hrpc.Call("guest.expose_port", req)
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Start local port proxy for the new port
	if hrpc.portProxy != nil {
		hrpc.portProxy.AddPort(req.GuestPort)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleGuestUnexpose unexposes a port via the host, then stops the local port proxy.
func handleGuestUnexpose(w http.ResponseWriter, r *http.Request, hrpc *harnessRPC) {
	guestPortStr := r.PathValue("guest_port")
	guestPort := 0
	fmt.Sscanf(guestPortStr, "%d", &guestPort)
	if guestPort <= 0 {
		writeGuestError(w, http.StatusBadRequest, "invalid guest_port")
		return
	}

	result, err := hrpc.Call("guest.unexpose_port", map[string]int{"guest_port": guestPort})
	if err != nil {
		writeGuestError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Stop local port proxy
	if hrpc.portProxy != nil {
		hrpc.portProxy.RemovePort(guestPort)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
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
