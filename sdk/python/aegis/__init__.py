"""Aegis SDK -- thin in-VM helper for agents running inside Aegis microVMs."""

from aegis.workspace import workspace_path, ensure_dirs
from aegis.secrets import get_secret, require_secret
from aegis import log

__version__ = "0.1.0"
