# Workspaces

## Overview

Workspaces are persistent, writable volumes mounted into VMs at `/workspace`. They survive VM termination and restarts. Each app gets its own workspace, created automatically on first `aegis app serve`.

## Guest Paths

Inside the VM, the workspace is mounted at `/workspace` via virtiofs from the host. The following subdirectory conventions exist:

- `/workspace/data/` -- persistent application data
- `/workspace/output/` -- artifacts and exports
- `/workspace/.cache/` -- caches, safe to delete

These paths are conventions, not enforced by the harness. You can write anywhere under `/workspace`.

## Host Paths

On the host, workspaces live at:

- `~/.aegis/data/workspaces/{appID}/` -- one directory per app

The `appID` is the internal ID (e.g., `app-1739893456`), not the app name. You can find the appID via `aegis app info myapp`. You can inspect workspace contents directly:

```bash
ls ~/.aegis/data/workspaces/app-173.../
```

## Lifecycle

- **Created**: automatically when `aegis app serve` is called.
- **Persists**: across VM pause/resume, terminate/restart, and daemon restart.
- **Deleted**: only when the app is deleted via `aegis app delete` (cascade).
- **NOT deleted**: on `aegis down` (daemon stop) or on VM terminate (idle timeout).

## When No Workspace Exists

Not all execution modes provide a workspace:

- **Task mode** (`aegis run -- cmd`): no workspace unless the task references an app.
- **Instance mode** (`aegis run --expose 80 -- cmd`): no workspace.
- **App serve mode** (`aegis app serve`): workspace always available.

To check from inside the guest:

```python
if os.path.isdir("/workspace")
```

## Retention and Backup

There is no automatic retention policy in M3. Workspaces grow unbounded.

To reclaim space, `aegis app delete myapp` removes the workspace. You can also manually delete files under `~/.aegis/data/workspaces/`.

There is no built-in backup. Treat workspace contents as you would any local data -- back up `~/.aegis/data/workspaces/` if needed.

GC and retention policies are planned for M5.

## Shared Workspaces (Future)

Shared workspaces are not implemented in M3. All workspaces are per-app (isolated mode).

The OpenClaw kit requires shared workspaces (multiple VMs mounting the same directory). This is planned for M5. When implemented, the kit manifest will specify `workspace.mode: shared`.

## Workspace vs Root Filesystem

Important invariant: workspace volumes are NEVER part of release overlays or snapshots. They are separate disk layers with separate lifecycles. Publishing a release never captures workspace state. Terminating a VM never touches the workspace.
