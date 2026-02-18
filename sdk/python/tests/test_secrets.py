"""Tests for aegis.secrets."""

import pytest

from aegis.secrets import AegisSecretError, get_secret, require_secret


def test_get_secret_returns_value(monkeypatch):
    monkeypatch.setenv("MY_API_KEY", "sk-test-123")
    assert get_secret("MY_API_KEY") == "sk-test-123"


def test_get_secret_returns_none_when_missing(monkeypatch):
    monkeypatch.delenv("NONEXISTENT_SECRET", raising=False)
    assert get_secret("NONEXISTENT_SECRET") is None


def test_require_secret_returns_value(monkeypatch):
    monkeypatch.setenv("DB_PASSWORD", "hunter2")
    assert require_secret("DB_PASSWORD") == "hunter2"


def test_require_secret_raises_when_missing(monkeypatch):
    monkeypatch.delenv("MISSING_SECRET", raising=False)
    with pytest.raises(AegisSecretError) as exc_info:
        require_secret("MISSING_SECRET")
    assert "MISSING_SECRET" in str(exc_info.value)


def test_require_secret_error_message_includes_guidance(monkeypatch):
    monkeypatch.delenv("SOME_KEY", raising=False)
    with pytest.raises(AegisSecretError) as exc_info:
        require_secret("SOME_KEY")
    msg = str(exc_info.value)
    assert "SOME_KEY" in msg
    assert "aegis secret set" in msg
