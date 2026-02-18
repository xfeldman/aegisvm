# Secrets

## Overview

Secrets are encrypted values stored in the Aegis registry and injected into VMs as environment variables at process start. They are never written to disk inside the VM.

## Storage

Secrets are encrypted at rest with AES-256-GCM and stored in `~/.aegis/data/aegis.db` (SQLite, `secrets` table). The encrypted value is formatted as `nonce || ciphertext`, with the 12-byte nonce prepended to the ciphertext.

The master key lives at `~/.aegis/master.key` (32 random bytes, auto-generated on first use). The master key file has permissions 0600.

Secret values are never exposed in API responses. List endpoints return names only.

## Scopes

Aegis supports two secret scopes:

- **per_app**: scoped to a single app. Set via `aegis secret set APP KEY VALUE`. Stored with `app_id` in the DB.
- **per_workspace**: shared across all apps. Set via `aegis secret set-workspace KEY VALUE`. Stored with `app_id = ""`.

Merge rule: workspace secrets are loaded first, then app secrets overlay on top. App-scoped secrets win on name collision.

## Injection

Secrets are decrypted on the host at VM boot or task creation time. They are merged into the `env` field of the `runTask` or `startServer` RPC. The harness passes them to the child process via `execve`. The agent reads them via `os.environ` or `process.env`.

Per-task env vars (from `aegis run` or the API) take precedence over secrets. If a key already exists in the explicit env, the secret does not overwrite it.

## What Aegis Guarantees

- Encrypted at rest (AES-256-GCM with local master key).
- Never written to disk inside the VM.
- Never included in any snapshot tier (base, release, or cached instance).
- Never returned in API responses (list shows names only).
- Re-injected on every cold boot from disk layers.

## What Aegis Does NOT Guarantee

- No key rotation. Deleting `~/.aegis/master.key` invalidates ALL stored secrets -- they become undecryptable. You must re-set all secrets after key deletion. Proper rotation (decrypt-all, re-encrypt) is a future concern.
- No audit logging of secret access.
- No per-run scoping (all app secrets are injected into every task/serve for that app).
- No hardware security module (HSM) integration -- the master key is a plain file.
- An agent process CAN leak secrets by logging them, writing them to `/workspace`, or sending them over the network. Aegis prevents disk persistence inside the VM but cannot prevent the process from exfiltrating its own env.

## Master Key

- **Location**: `~/.aegis/master.key` (configurable via `config.MasterKeyPath`).
- **Auto-generated** on first use (32 bytes from `crypto/rand`).
- **File permissions**: 0600; directory: 0700.
- **If deleted**: all stored secrets are lost. No recovery.
- **If compromised**: an attacker can decrypt all secrets in the SQLite database.
- **Backup**: copy `~/.aegis/master.key` to a secure location. Restore by placing it back.

## CLI Commands

Quick reference. See CLI.md for full documentation.

| Command | Description |
|---------|-------------|
| `aegis secret set APP KEY VALUE` | Set app-scoped secret |
| `aegis secret list APP` | List secret names (no values) |
| `aegis secret delete APP KEY` | Delete app secret |
| `aegis secret set-workspace KEY VALUE` | Set workspace-wide secret |
| `aegis secret list-workspace` | List workspace secret names |

## Snapshot Restore and Secrets (M4+)

In the current model (M3), "restore" means cold boot from disk layers. Secrets are re-injected via the `startServer` RPC `env` field on every boot. This works because the harness is a fresh process.

When memory snapshot restore arrives (M4+), the harness will already be running and cannot receive secrets via `startServer`. Two approaches are under consideration: a dedicated `injectSecrets` RPC, or a "restart server process with env" contract. See IMPLEMENTATION_KICKOFF.md section 10.1 for details.
