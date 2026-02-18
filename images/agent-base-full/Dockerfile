# agent-base-full — Python + Node.js environment for Aegis agents
#
# Includes both Python and Node.js runtimes plus common dev tools.
# Heavier than agent-base-python (~200MB vs ~80MB) but more versatile.

FROM python:3.12-alpine

RUN apk add --no-cache curl wget git openssh-client jq \
    nodejs npm

WORKDIR /workspace
