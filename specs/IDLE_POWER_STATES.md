# Idle Detection and Power States

**Post-implementation specification — documents the idle detection, activity heartbeat, keepalive lease, and power state model.**

**Date:** 2026-02-21
**Depends on:** [GVPROXY_NETWORKING.md](GVPROXY_NETWORKING.md) (in-process gvproxy, vsock control channel)

---

## 1. Problem

The idle timer only tracked inbound TCP connections. Any workload with outbound-only activity (Telegram polling, npm install, LLM API calls, background processing) was paused after 60 seconds, incorrectly treated as idle.

## 2. Power State Model

```
STOPPED ──boot──→ RUNNING ──idle timeout──→ PAUSED ──stop timeout──→ STOPPED
                     ↑                         │
                     └────wake-on-connect───────┘
```

| State | VM | Network | CPU | Memory |
|-------|-----|---------|-----|--------|
| RUNNING | Active | Full | Active | Allocated |
| PAUSED | SIGSTOP | Frozen (gvproxy in-process) | Zero | Allocated |
| STOPPED | Dead | None | Zero | Freed |

**Transitions:**
- RUNNING → PAUSED: idle timer fires (no inbound connections, no activity heartbeat, no lease)
- PAUSED → RUNNING: wake-on-connect (inbound TCP to exposed port)
- PAUSED → STOPPED: stop timer fires (paused too long, default 5 min)
- STOPPED → RUNNING: explicit `instance start` (full boot)

## 3. Activity Signals

Three independent signals prevent the idle timer from firing. The instance stays RUNNING as long as ANY signal is active:

### 3.1 Inbound Connections (existing)

Tracked by the router. When `activeConns > 0`, the idle timer doesn't start. When the last connection closes, the timer begins.

### 3.2 Activity Heartbeat (new — harness-side)

The harness sends periodic `"activity"` JSON-RPC notifications over the vsock control channel when the guest has meaningful work:

```json
{"jsonrpc":"2.0","method":"activity","params":{"tcp":3,"cpu_ms":120,"net_bytes":4096}}
```

**Metrics probed every 5 seconds (±500ms jitter):**

| Metric | Source | Threshold | What it catches |
|--------|--------|-----------|-----------------|
| `tcp` | `/proc/net/tcp` + `/proc/net/tcp6`, state `01` (ESTABLISHED) | > 0 | Outbound API calls, Telegram polling, WebSocket connections |
| `cpu_ms` | `/proc/{pid}/stat` utime+stime delta × 10 | > 0 | npm install, compilation, agent execution |
| `net_bytes` | `/sys/class/net/eth0/statistics/{tx,rx}_bytes` delta | > 512 | Downloads, uploads, QUIC/UDP traffic |

**Design properties:**
- **Best-effort, not authoritative** — prevents pausing mid-work, not a reliability guarantee
- **Silence = idle** — when all metrics are below threshold, no notification is sent, and the idle timer runs naturally
- **512-byte net_bytes threshold** — filters background ARP/keepalive noise (~70 bytes/5s) from the gvproxy virtual network stack
- **CPU as delta** — absolute ticks are meaningless; only the change since last sample matters
- **Jitter** — ±500ms prevents synchronizing multiple VMs on the same host

### 3.3 Keepalive Lease (new — kit-facing)

Kits can explicitly prevent pause by acquiring a lease with a TTL:

```json
{"jsonrpc":"2.0","method":"keepalive","params":{"ttl_ms":30000,"reason":"build"}}
{"jsonrpc":"2.0","method":"keepalive.release","params":{}}
```

**Semantics:**
- While a lease is held, the instance will NOT pause (even with 0 connections and no heartbeats)
- Leases auto-expire after `ttl_ms` — no stuck-alive instances if the kit crashes
- `reason` is for logging/debugging (visible in instance info API)
- `keepalive.release` explicitly drops the lease before TTL expiry
- Renew by sending another `keepalive` with a new TTL (resets the expiry timer)

**Use cases:**
- Build agent acquires lease during `npm install` → releases after install
- Cron kit acquires lease during scheduled task execution
- Interactive session kit acquires lease while user is connected

## 4. Idle Policy (kit-facing)

Per-instance `idle_policy` controls which signals prevent pause:

| Policy | Heartbeat | Lease | Inbound connections |
|--------|-----------|-------|---------------------|
| `"default"` (or empty) | Yes | Yes | Yes |
| `"leases_only"` | **No** | Yes | Yes |

**Why `leases_only` exists:** Some kits run background daemons that always have outbound connections or periodic CPU activity. Without `leases_only`, these would never pause. The kit should use explicit leases to express "I'm doing real work" vs "I'm just polling."

**Not user-facing.** This is a kit implementation detail. The API field exists but is not exposed in CLI help, MCP tool descriptions, or user documentation. Kits set it at instance creation time.

## 5. Implementation

### Files

| File | What |
|------|------|
| `internal/harness/activity_linux.go` | Guest-side probes: `countEstablishedTCP()`, `processUsedCPUTicks()`, `ethByteCounters()` |
| `internal/harness/activity_other.go` | Stubs for non-Linux builds |
| `internal/harness/rpc.go` | `monitorActivity()` goroutine, started after primary process launch |
| `internal/lifecycle/manager.go` | `bumpActivity()`, `acquireLease()`, `releaseLease()`, `IdlePolicy` field, demuxer handlers |
| `internal/api/server.go` | `idle_policy` in create request, `lease_held`/`lease_reason`/`lease_expires_at` in instance info |

### Demuxer notification handlers

```go
case "activity":     → m.bumpActivity(inst)
case "keepalive":    → m.acquireLease(inst, ttlMs, reason)
case "keepalive.release": → m.releaseLease(inst)
```

### Pause guard

```go
func (m *Manager) pauseInstance(inst *Instance) {
    if inst.State != StateRunning || inst.activeConns > 0 || inst.leaseHeld {
        return // don't pause
    }
    // ... SIGSTOP
}
```

## 6. Interaction Matrix

| Scenario | Inbound | Heartbeat | Lease | Result |
|----------|---------|-----------|-------|--------|
| HTTP server with active requests | Yes | Yes | No | Running (inbound) |
| HTTP server, no requests | No | No | No | Pauses after 60s |
| Telegram bot polling | No | Yes (tcp>0) | No | Running (heartbeat) |
| npm install | No | Yes (cpu>0) | No | Running (heartbeat) |
| sleep 300 | No | No | No | Pauses after 60s |
| Build with explicit lease | No | Maybe | Yes | Running (lease) |
| Bot with leases_only policy, polling | No | Ignored | No | Pauses after 60s |
| Bot with leases_only, lease held | No | Ignored | Yes | Running (lease) |

## 7. Future Work

- **Per-instance idle timeout override** — different workloads may want different idle windows (not just 60s)
- **Promote idle_policy to user-facing** if kits prove the model is stable
- **Persist leases across daemon restart** — currently leases are lost on aegisd restart (instance comes back stopped anyway)
- **Activity telemetry** — expose heartbeat metrics via API for monitoring/debugging
