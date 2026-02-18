"""Structured JSON logging to stdout/stderr for Aegis agents.

Each function emits exactly one JSON line.
"""

from __future__ import annotations

import json
import sys
from datetime import datetime, timezone


def _emit(stream, level: str, msg: str, **kwargs) -> None:
    """Write a single JSON log line to *stream*."""
    record = {
        "level": level,
        "msg": msg,
        "ts": datetime.now(timezone.utc).isoformat(),
    }
    record.update(kwargs)
    stream.write(json.dumps(record, ensure_ascii=False) + "\n")


def info(msg: str, **kwargs) -> None:
    """Log info-level message as JSON to stdout."""
    _emit(sys.stdout, "info", msg, **kwargs)


def warn(msg: str, **kwargs) -> None:
    """Log warning-level message as JSON to stdout."""
    _emit(sys.stdout, "warn", msg, **kwargs)


def error(msg: str, **kwargs) -> None:
    """Log error-level message as JSON to stderr."""
    _emit(sys.stderr, "error", msg, **kwargs)


def debug(msg: str, **kwargs) -> None:
    """Log debug-level message as JSON to stdout."""
    _emit(sys.stdout, "debug", msg, **kwargs)
