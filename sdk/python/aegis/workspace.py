"""Workspace path helpers for Aegis agents."""

from __future__ import annotations

import os


def workspace_path() -> str:
    """Return the workspace root.

    Resolution order:
    1. AEGIS_WORKSPACE_PATH environment variable (explicit override).
    2. /workspace -- the conventional mount point inside an Aegis microVM.
    3. ./workspace -- fallback for local development outside a VM.
    """
    env = os.environ.get("AEGIS_WORKSPACE_PATH")
    if env:
        return env
    if os.path.isdir("/workspace"):
        return "/workspace"
    return "./workspace"


def ensure_dirs() -> None:
    """Create the standard workspace subdirectories: data/, output/, .cache/."""
    root = workspace_path()
    os.makedirs(os.path.join(root, "data"), exist_ok=True)
    os.makedirs(os.path.join(root, "output"), exist_ok=True)
    os.makedirs(os.path.join(root, ".cache"), exist_ok=True)


def data_path() -> str:
    """Return workspace_path()/data."""
    return os.path.join(workspace_path(), "data")


def output_path() -> str:
    """Return workspace_path()/output."""
    return os.path.join(workspace_path(), "output")


def cache_path() -> str:
    """Return workspace_path()/.cache."""
    return os.path.join(workspace_path(), ".cache")
