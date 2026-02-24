# Vsock Control Channel Lifecycle — Linux/Cloud Hypervisor

## Problem

On Linux (Cloud Hypervisor), the vsock control channel between aegisd and the guest harness breaks during pause and snapshot/restore cycles. The harness has no reliable way to detect the broken channel and reconnect.

On macOS (libkrun), this problem doesn't exist: `SIGSTOP` freezes the entire vmm-worker process including the Go runtime, gvproxy, and kernel vsock state. The connection survives intact — no bytes lost, no reconnect needed. `SIGCONT` resumes everything.

## Root Cause

Cloud Hypervisor `vm.pause` freezes guest vCPUs but the host-side daemon keeps running. The daemon's vsock connection goes silent (no reads, no writes). After `vm.resume`, the connection should still be valid — same CH process, same vsock device. However:

1. The **harness** side is frozen during pause. If SO_RCVTIMEO is set, the timeout fires immediately after resume (time advanced while frozen), causing a false disconnect.
2. For **snapshot/restore**, the CH process is killed and a new one started. The guest-side vsock fd points to a dead CH process. CH does not send `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET` (unlike Firecracker), so the guest kernel thinks the connection is alive. Reads on the dead fd block forever.

## Design Principle

**The daemon controls the lifecycle, not the harness.** The daemon knows exactly when it's pausing, snapshotting, stopping, and restoring. The harness should never guess via timeouts.

## macOS Model (Reference — Correct and Stable)

```
Pause:   SIGSTOP → everything frozen → connection survives → SIGCONT → resume
Stop:    N/A (PersistentPause=true, VMs stay paused forever, OS manages swap)
Restore: N/A (no snapshots, no cold restart)
```

The control channel is established once at boot and never re-established. `inst.Channel` and `inst.demuxer` survive the entire instance lifetime.

## Linux Model (Proposed)

### Pause

```
1. daemon: demuxer.Call("prepare_pause") → harness acks
2. daemon: vmm.PauseVM(handle) → vm.pause freezes vCPUs
3. connection stays open, both sides frozen/idle
```

The `prepare_pause` RPC tells the harness to stop sending data (activity probes, log flushes). This ensures no in-flight data when the pause happens. The vsock connection remains open — the kernel buffers it on both sides.

**No channel close, no reconnect on resume.**

### Resume

```
1. daemon: vmm.ResumeVM(handle) → vm.resume unfreezes vCPUs
2. harness resumes where it left off
3. daemon: demuxer continues using existing channel
4. (optional) daemon: demuxer.Call("resumed") → harness restarts activity probes
```

The existing `inst.Channel` and `inst.demuxer` are reused. Same as macOS.

### Snapshot + Stop (idle timeout, PersistentPause=false)

```
1. daemon: demuxer.Call("prepare_stop") → harness acks, enters reconnect-wait mode
2. daemon: demuxer.Stop() → closes the demuxer, closes the connection
3. harness: sees EOF on the connection → enters reconnect loop (blocking dial)
4. daemon: vmm.PauseVM → vm.pause
5. daemon: vmm.SnapshotVM → vm.snapshot (memory saved to disk)
6. daemon: vmm.StopVM → kills CH process, cleans tap/NAT/sockets
7. inst.Channel = nil, inst.demuxer = nil, inst.State = stopped
```

Key: the daemon **explicitly closes the channel** (step 2) before pausing. The harness sees a clean EOF (not a timeout, not a broken pipe) and enters the reconnect loop. The snapshot at step 5 captures the harness in its reconnect-wait state — it's blocked on `dialVsock()` which will fail immediately (no CH process), so it retries with backoff.

### Restore (cold restart from snapshot)

```
1. daemon: vmm.CreateVM → new VM ID, new sockets
2. daemon: reads snapshot config.json → gets original vsock socket path
3. daemon: creates vsock listener at original path
4. daemon: vmm.StartVM → starts CH, calls vm.restore + vm.resume
5. harness: wakes from snapshot, still in reconnect loop
6. harness: dialVsock succeeds → new connection established
7. daemon: acceptHarness succeeds → new ControlChannel returned
8. daemon: creates new demuxer, sends run RPC (re-handoff)
9. inst.Channel = newChannel, inst.demuxer = newDemux, inst.State = running
```

Key: the harness was saved in its reconnect-wait state (step 5). After restore, the vsock device is available again (new CH process, same vsock CID). The reconnect dial succeeds. The daemon accepts the new connection and re-establishes the demuxer.

## Implementation Changes

### 1. New harness RPC: `prepare_pause` / `prepare_stop`

In `internal/harness/rpc.go`, add handlers:

```go
case "prepare_pause":
    // Stop activity probes, flush pending writes.
    // Ack immediately — the daemon will pause vCPUs after receiving the ack.
    respond(id, map[string]string{"status": "ready"})

case "prepare_stop":
    // Same as prepare_pause, but the harness knows a stop is coming.
    // After acking, the harness should expect the connection to close
    // and enter the reconnect loop.
    respond(id, map[string]string{"status": "ready"})
```

### 2. Lifecycle manager: close channel before snapshot

In `stopIdleInstance()`, before calling `vmm.PauseVM`:

```go
// Tell harness to prepare for stop
if inst.demuxer != nil {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    inst.demuxer.Call(ctx, "prepare_stop", nil, nextRPCID())
    cancel()
}

// Close the channel — harness sees EOF, enters reconnect loop
if inst.demuxer != nil {
    inst.demuxer.Stop()
}

// Now pause and snapshot — harness is in reconnect-wait state
m.vmm.PauseVM(handle)
snapshotter.SnapshotVM(handle, snapshotDir)
m.vmm.StopVM(handle)
```

### 3. Remove SO_RCVTIMEO from vsock

In `internal/harness/vsock_linux.go`, remove the `SO_RCVTIMEO` setsockopt. The timeout is not needed — the daemon explicitly closes the connection when stopping. For snapshot/restore, the harness is already in the reconnect loop (saved in that state by the snapshot).

### 4. Harness reconnect loop: only on explicit disconnect

In `internal/harness/main.go`, the reconnect loop already fires when `handleConnection` returns (connection closed). No timeout-based detection needed. The `reconnectVsock()` function retries indefinitely with backoff — correct for the restore case where the host may take time to start a new CH process.

### 5. Restore path: use original vsock socket path

Already implemented in `cloudhv.go` — `readSnapshotVsockPath()` reads the vsock path from the snapshot's `config.json` and creates the listener at that path.

## State Diagram

```
                    boot
                     │
                     ▼
                 ┌────────┐
         ┌──────│ RUNNING │◄──────────────────────┐
         │      └────┬────┘                        │
         │           │                             │
    idle timeout     │                        resume (SIGCONT on mac,
         │           │                         vm.resume on linux)
         ▼           │                             │
    ┌────────┐       │                        ┌────┴────┐
    │ PAUSED │───────┼── resume ──────────────│ PAUSED  │
    └────┬───┘       │                        └─────────┘
         │           │
  stop timeout       │  (linux only, PersistentPause=false)
  (linux only)       │
         │           │
         ▼           │
  prepare_stop RPC   │
  close channel      │
  snapshot           │
  kill CH            │
         │           │
         ▼           │
    ┌─────────┐      │
    │ STOPPED │──────┘ (cold restart → restore from snapshot)
    └─────────┘
```

## What Doesn't Change

- macOS path: untouched. SIGSTOP/SIGCONT, PersistentPause=true, no snapshots.
- Initial boot control channel setup: same on both platforms.
- Harness RPC protocol: additive (new methods, no breaking changes).
- VMM interface: no changes. `PauseVM`/`ResumeVM`/`StopVM` stay as-is.
- Router/ingress: unaffected.
