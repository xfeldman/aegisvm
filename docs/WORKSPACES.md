# Workspaces

## Overview

Workspaces are persistent, writable volumes mounted into VMs at `/workspace`. They survive VM termination and restarts.

## Guest Paths

Inside the VM, the workspace is mounted at `/workspace` via virtiofs from the host. The following subdirectory conventions exist:

- `/workspace/data/` -- persistent application data
- `/workspace/output/` -- artifacts and exports
- `/workspace/.cache/` -- caches, safe to delete

These paths are conventions, not enforced by the harness. You can write anywhere under `/workspace`.

## Host Paths

On the host, workspaces live at:

- `~/.aegis/data/workspaces/`

You can inspect workspace contents directly on the host.

## Lifecycle

- **Created**: when an instance is started with `--workspace` flag.
- **Persists**: across VM pause/resume, stop/restart, and daemon restart.
- **Deleted**: only when explicitly removed by the user.
- **NOT deleted**: on `aegis down` (daemon stop) or on VM stop (idle timeout).

## When No Workspace Exists

Not all instances have a workspace. A workspace is only available when explicitly configured via `--workspace` at instance creation.

To check from inside the guest:

```python
if os.path.isdir("/workspace")
```

## Retention and Backup

There is no automatic retention policy. Workspaces grow unbounded.

To reclaim space, manually delete files under `~/.aegis/data/workspaces/`.

There is no built-in backup. Treat workspace contents as you would any local data.

GC and retention policies are planned for a future milestone.

## Shared Workspaces (Future)

Shared workspaces are not implemented. All workspaces are currently per-instance (isolated mode).

Shared workspaces (multiple VMs mounting the same directory) are planned for M5.

## Workspace vs Root Filesystem

Important invariant: workspace volumes are NEVER part of rootfs overlays or snapshots. They are separate disk layers with separate lifecycles.
