"""Tests for aegis.log."""

import io
import json
import sys

from aegis import log


def test_info_writes_json_to_stdout(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)

    log.info("hello world")

    line = buf.getvalue()
    record = json.loads(line)
    assert record["level"] == "info"
    assert record["msg"] == "hello world"
    assert "ts" in record


def test_warn_writes_json_to_stdout(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)

    log.warn("careful")

    record = json.loads(buf.getvalue())
    assert record["level"] == "warn"
    assert record["msg"] == "careful"


def test_error_writes_json_to_stderr(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stderr", buf)

    log.error("something broke")

    record = json.loads(buf.getvalue())
    assert record["level"] == "error"
    assert record["msg"] == "something broke"


def test_debug_writes_json_to_stdout(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)

    log.debug("verbose detail")

    record = json.loads(buf.getvalue())
    assert record["level"] == "debug"
    assert record["msg"] == "verbose detail"


def test_extra_kwargs_included(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)

    log.info("request done", status=200, duration_ms=42)

    record = json.loads(buf.getvalue())
    assert record["status"] == 200
    assert record["duration_ms"] == 42


def test_info_does_not_write_to_stderr(monkeypatch):
    stdout_buf = io.StringIO()
    stderr_buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", stdout_buf)
    monkeypatch.setattr(sys, "stderr", stderr_buf)

    log.info("only stdout")

    assert stdout_buf.getvalue() != ""
    assert stderr_buf.getvalue() == ""


def test_error_does_not_write_to_stdout(monkeypatch):
    stdout_buf = io.StringIO()
    stderr_buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", stdout_buf)
    monkeypatch.setattr(sys, "stderr", stderr_buf)

    log.error("only stderr")

    assert stderr_buf.getvalue() != ""
    assert stdout_buf.getvalue() == ""


def test_output_is_single_line(monkeypatch):
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)

    log.info("one line")

    lines = buf.getvalue().splitlines()
    assert len(lines) == 1
