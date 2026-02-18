"""Tests for aegis.workspace."""

import os

from aegis.workspace import (
    cache_path,
    data_path,
    ensure_dirs,
    output_path,
    workspace_path,
)


def test_workspace_path_env_override(monkeypatch, tmp_path):
    """AEGIS_WORKSPACE_PATH env var takes precedence over all other heuristics."""
    custom = str(tmp_path / "custom")
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", custom)
    assert workspace_path() == custom


def test_workspace_path_local_fallback(monkeypatch):
    """When no env var is set and /workspace does not exist, fall back to ./workspace."""
    monkeypatch.delenv("AEGIS_WORKSPACE_PATH", raising=False)
    # On a normal dev machine /workspace should not exist.
    if not os.path.isdir("/workspace"):
        assert workspace_path() == "./workspace"


def test_ensure_dirs_creates_subdirectories(monkeypatch, tmp_path):
    """ensure_dirs() creates data/, output/, and .cache/ under the workspace root."""
    root = str(tmp_path / "ws")
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", root)

    ensure_dirs()

    assert os.path.isdir(os.path.join(root, "data"))
    assert os.path.isdir(os.path.join(root, "output"))
    assert os.path.isdir(os.path.join(root, ".cache"))


def test_ensure_dirs_idempotent(monkeypatch, tmp_path):
    """Calling ensure_dirs() twice does not raise."""
    root = str(tmp_path / "ws")
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", root)

    ensure_dirs()
    ensure_dirs()  # must not raise


def test_data_path(monkeypatch, tmp_path):
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", str(tmp_path))
    assert data_path() == os.path.join(str(tmp_path), "data")


def test_output_path(monkeypatch, tmp_path):
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", str(tmp_path))
    assert output_path() == os.path.join(str(tmp_path), "output")


def test_cache_path(monkeypatch, tmp_path):
    monkeypatch.setenv("AEGIS_WORKSPACE_PATH", str(tmp_path))
    assert cache_path() == os.path.join(str(tmp_path), ".cache")
