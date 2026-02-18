"""Secret access helpers for Aegis agents."""

from __future__ import annotations

import os


class AegisSecretError(Exception):
    """Raised when a required secret is not available."""


def get_secret(name: str) -> str | None:
    """Get a secret by name. Returns None if not set."""
    return os.environ.get(name)


def require_secret(name: str) -> str:
    """Get a secret by name. Raises AegisSecretError if not set."""
    try:
        return os.environ[name]
    except KeyError:
        raise AegisSecretError(
            f"Secret '{name}' not available. "
            "Ensure it is set via 'aegis secret set' and the task/app references it."
        )
