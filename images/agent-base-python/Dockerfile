# agent-base-python — Minimal Python environment for Aegis agents
#
# Usage:
#   docker build -t agent-base:python .
#   aegis run --name web --image agent-base:python --expose 80 -- python server.py
#
# Note: aegis-harness is injected automatically when preparing the instance rootfs.
# You do NOT need to include it in your image. Aegis ignores ENTRYPOINT and CMD —
# the harness is always PID 1 inside the VM.

FROM python:3.12-alpine

RUN apk add --no-cache curl wget

# Install aegis-sdk for workspace/secrets/logging helpers
# When published to PyPI: pip install aegis-sdk
# For now, the SDK can be copied manually into images
# COPY aegis /usr/lib/python3.12/site-packages/aegis

WORKDIR /workspace
