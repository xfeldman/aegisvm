# Secrets

## Overview

Secrets are encrypted values stored in the Aegis registry and injected into VMs as environment variables at process start. They are never written to disk inside the VM.

## Storage

Secrets are encrypted at rest with AES-256-GCM and stored in `~/.aegis/data/aegis.db` (SQLite, `secrets` table). The encrypted value is formatted as `nonce || ciphertext`, with the 12-byte nonce prepended to the ciphertext.

The master key lives at `~/.aegis/master.key` (32 random bytes, auto-generated on first use). The master key file has permissions 0600.

Secret values are never exposed in API responses. List endpoints return names only.

## Model

Secrets are a flat key-value store. No scoping, no naming conventions, no rotation policy. Core provides dumb infrastructure plumbing: store, encrypt, inject.

## Injection

Secrets are **not injected by default**. Each instance explicitly declares which
secrets it receives via the `--env` flag:

```bash
# Inject specific secrets (bare key = secret lookup)
aegis run --env API_KEY --env DB_URL -- python app.py

# Inject with mapped secret name
aegis run --env API_KEY=secret.my_api_key -- python app.py

# Inject all secrets
aegis run --env '*' -- python agent.py

# Mix secrets and literal values
aegis run --env API_KEY --env DEBUG=true -- python app.py

# No --env flag = no secrets injected
aegis run -- echo hello
```

The `--env` flag supports three forms:
- `--env KEY` — bare key, shorthand for `--env KEY=secret.KEY` (secret lookup)
- `--env KEY=secret.name` — mapped secret reference (inject secret `name` as env var `KEY`)
- `--env KEY=value` — literal value (no secret lookup)

At boot time, matching secrets are decrypted on the host and injected as env vars
via the `run` RPC. The harness passes them to the child process via `execve`.

## What Aegis Guarantees

- Encrypted at rest (AES-256-GCM with local master key).
- Never written to disk inside the VM.
- Never included in any snapshot tier.
- Never returned in API responses (list shows names only).
- Re-injected on every cold boot from disk layers.

## What Aegis Does NOT Guarantee

- No key rotation. Deleting `~/.aegis/master.key` invalidates ALL stored secrets -- they become undecryptable. You must re-set all secrets after key deletion.
- No audit logging of secret access.
- No hardware security module (HSM) integration -- the master key is a plain file.
- An agent process CAN leak secrets by logging them, writing them to `/workspace`, or sending them over the network.

## Master Key

- **Location**: `~/.aegis/master.key` (configurable via `config.MasterKeyPath`).
- **Auto-generated** on first use (32 bytes from `crypto/rand`).
- **File permissions**: 0600; directory: 0700.
- **If deleted**: all stored secrets are lost. No recovery.
- **If compromised**: an attacker can decrypt all secrets in the SQLite database.
- **Backup**: copy `~/.aegis/master.key` to a secure location. Restore by placing it back.

## CLI Commands

| Command | Description |
|---------|-------------|
| `aegis secret set KEY VALUE` | Set or update a secret |
| `aegis secret list` | List secret names (no values) |
| `aegis secret delete KEY` | Delete a secret |

## Snapshot Restore and Secrets (Future)

In the current model, "restore" means cold boot from disk layers. Secrets are re-injected via the `run` RPC `env` field on every boot.

When memory snapshot restore arrives, the harness will already be running and cannot receive secrets via `run`. A dedicated `injectSecrets` RPC or "restart process with env" contract will be needed.
