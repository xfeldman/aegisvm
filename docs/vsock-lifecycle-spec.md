# Vsock Control Channel Lifecycle — Linux/Cloud Hypervisor

## Problem

On Linux (Cloud Hypervisor), the vsock control channel between aegisd and the guest harness breaks during snapshot/restore cycles. The harness has no reliable way to detect the broken channel and reconnect.

On macOS (libkrun), this problem doesn't exist: `SIGSTOP` freezes the entire vmm-worker process including the Go runtime, gvproxy, and kernel vsock state. The connection survives intact. `SIGCONT` resumes everything.

## Design Principle

**The daemon controls the lifecycle, not the harness.** The daemon knows exactly when it's pausing, snapshotting, stopping, and restoring. The harness never guesses via timeouts. The host may die without signaling the guest; therefore we never depend on guest-side detection.

## macOS Model (Reference — Correct and Stable)

```
Pause:   SIGSTOP → everything frozen → connection survives → SIGCONT → resume
Stop:    N/A (PersistentPause=true, VMs stay paused forever, OS manages swap)
Restore: N/A (no snapshots, no cold restart)
```

The control channel is established once at boot and never re-established.

## Linux Model

### Channel Generation

Every control channel has a monotonically increasing `channel_gen` (uint64), stored in the lifecycle manager's `Instance` struct. Incremented each time a new `ControlChannel` is established after restore. Included in:

- `quiesce.stop` RPC (daemon → harness): "you are on gen N, prepare for disconnect"
- Harness reconnect hello (harness → daemon): "I am reconnecting, last gen was N"

This discriminates stale events from old channels and prevents races when stop is triggered twice or a reconnect races with a mid-transition manager.

### Pause

```
1. daemon: vmm.PauseVM(handle) → vm.pause freezes vCPUs
2. connection stays open, both sides frozen/idle
```

No channel close, no reconnect on resume. The vsock connection remains open — same CH process, same vsock device. Same invariant as macOS.

`prepare_pause` is not implemented now. If activity probes or tether streaming create confusing artifacts after resume, we add `quiesce.pause` later as a barrier with the same contract as `quiesce.stop`.

### Resume

```
1. daemon: vmm.ResumeVM(handle) → vm.resume unfreezes vCPUs
2. harness resumes where it left off
3. daemon: demuxer continues using existing channel
```

Existing `inst.Channel` and `inst.demuxer` are reused. Same as macOS.

### Snapshot + Stop (idle timeout, PersistentPause=false)

```
1. daemon: demuxer.Call("quiesce.stop", {channel_gen: N}) → harness acks
2. daemon: demuxer.Stop() → closes the demuxer, closes the connection
3. harness: sees EOF → enters reconnect loop (blocking dial with backoff)
4. daemon: vmm.PauseVM → vm.pause
5. daemon: vmm.SnapshotVM → vm.snapshot (memory saved to disk)
6. daemon: vmm.StopVM → kills CH process, cleans tap/NAT/sockets
7. inst.Channel = nil, inst.demuxer = nil, inst.State = stopped
```

**`quiesce.stop` contract:**
- After ACK, the harness enters quiescing mode: no new optional traffic (activity probes, tether deltas, log flushes) will be intentionally emitted.
- ACK does NOT guarantee delivery of any subsequent guest→host frames. Anything sent after ACK may be dropped.
- The harness does NOT stop the primary process. The app inside the VM keeps running — it will be frozen by `vm.pause` and captured in the snapshot.

The daemon closes the channel (step 2) before pausing. The harness sees a clean EOF and enters the reconnect loop. The snapshot at step 5 captures the harness in its reconnect-wait state.

### Restore (cold restart from snapshot)

```
1. daemon: vmm.CreateVM → new VM ID, new sockets
2. daemon: reads snapshot config.json → gets original vsock socket path
3. daemon: creates vsock listener at original path (_5000 suffix)
4. daemon: vmm.StartVM → starts CH, calls vm.restore + vm.resume
5. harness: wakes from snapshot, already in reconnect loop (saved in that state)
6. harness: dialVsock succeeds → sends reconnect hello {last_gen: N}
7. daemon: acceptHarness → new ControlChannel, channel_gen = N+1
8. daemon: creates new demuxer, sends run RPC (re-handoff)
9. inst.Channel = newChannel, inst.demuxer = newDemux, inst.State = running
```

The harness was saved in its reconnect-wait state (step 5). After restore, the vsock device is available again (new CH process, same vsock CID). The reconnect dial succeeds.

### Reconnect Loop (harness side)

The harness reconnect loop retries indefinitely with capped exponential backoff:

- Initial delay: 500ms
- Backoff: 1.5x per attempt
- Cap: 5s max delay
- No retry limit — restore may take time

The primary process inside the VM **keeps running** while disconnected. This is the point of snapshot — the app state is preserved. The harness just can't communicate with the host until the channel is re-established.

## Implementation Changes

### 1. Channel generation on Instance

In `internal/lifecycle/manager.go`, add to `Instance`:

```go
channelGen uint64 // monotonically increasing, incremented on each new ControlChannel
```

Increment in `bootInstance` after `StartVM` returns the channel, and again in the restore path.

### 2. `quiesce.stop` RPC

In `internal/harness/rpc.go`, add handler:

```go
case "quiesce.stop":
    // Daemon is about to close the channel, snapshot, and stop.
    // Stop optional traffic. The primary process keeps running.
    // After ACK, any frames we send may be dropped.
    hrpc.quiesced = true
    respond(id, map[string]string{"status": "ready"})
```

The `quiesced` flag suppresses activity probes and optional notifications. The flag is irrelevant after restore (new connection, fresh harness RPC state).

### 3. Close channel before snapshot in stopIdleInstance

In `internal/lifecycle/manager.go`, `stopIdleInstance()`:

```go
// Tell harness to quiesce
if inst.demuxer != nil {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    inst.demuxer.Call(ctx, "quiesce.stop", map[string]interface{}{
        "channel_gen": inst.channelGen,
    }, nextRPCID())
    cancel()
}

// Close channel — harness sees EOF, enters reconnect loop
if inst.demuxer != nil {
    inst.demuxer.Stop()
}
inst.Channel = nil
inst.demuxer = nil

// Now pause, snapshot, stop — harness is in reconnect-wait state
m.vmm.PauseVM(handle)
snapshotter.SnapshotVM(handle, snapshotDir)
m.vmm.StopVM(handle)
```

### 4. Remove SO_RCVTIMEO from vsock

In `internal/harness/vsock_linux.go`, remove `SO_RCVTIMEO` setsockopt. Not needed — the daemon explicitly closes the connection.

### 5. Restore path: use original vsock socket path

Already implemented — `readSnapshotVsockPath()` reads the path from the snapshot's `config.json`.

### 6. Harness reconnect hello

When the harness reconnects after restore, the first message it sends includes its last known `channel_gen`. The daemon validates this matches expectations and establishes the new channel with `gen+1`.

## State Diagram

```
                    boot
                     │
                     ▼
                 ┌────────┐
         ┌──────│ RUNNING │◄──────────────────────┐
         │      └────┬────┘                        │
         │           │                             │
    idle timeout     │                        vm.resume
         │           │                        (channel survives,
         ▼           │                         same as macOS)
    vm.pause         │                             │
         │           │                        ┌────┴────┐
         ├───────────┼── vm.resume ───────────│ PAUSED  │
         │           │                        └─────────┘
         │           │
  stop timeout       │  (linux only, PersistentPause=false)
  (linux only)       │
         │           │
         ▼           │
  quiesce.stop RPC   │
  demuxer.Stop()     │  ← channel closed, harness sees EOF
  vm.pause           │
  vm.snapshot        │
  kill CH            │
         │           │
         ▼           │
    ┌─────────┐      │
    │ STOPPED │──────┘  restore from snapshot
    └─────────┘         (new CH, new channel, gen+1)
```

## What Doesn't Change

- macOS path: untouched. SIGSTOP/SIGCONT, PersistentPause=true, no snapshots.
- Initial boot control channel setup: same on both platforms.
- Harness RPC protocol: additive (new method, no breaking changes).
- VMM interface: no changes. PauseVM/ResumeVM/StopVM stay as-is.
- Router/ingress: unaffected.
- Pause/resume path: no channel changes, same as macOS invariant.
