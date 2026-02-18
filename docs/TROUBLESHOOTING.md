# Troubleshooting

Searchable FAQ for common Aegis issues. Each entry states the problem, explains why it happens, and provides a fix.

---

## "Why can't I write to /etc (or /usr, /bin, /home)?"

**Why:** The root filesystem is remounted read-only by the harness at boot (`MS_REMOUNT|MS_RDONLY`). This protects the release rootfs from modification so that every boot starts from a known-good state.

**Fix:** Write to `/workspace` for persistent data, or `/tmp` for ephemeral scratch. Install everything you need at image build time in your Dockerfile. The root filesystem is your immutable base; the workspace is your mutable layer.

---

## "Why can't I curl localhost:8000 from the host?"

**Why:** The VM has its own network namespace. `localhost` inside the VM is not the host. The guest and host do not share a loopback interface.

**Fix:** All ingress goes through the Aegis router at `127.0.0.1:8099`. Use `curl http://127.0.0.1:8099/` to reach your app, or `curl http://127.0.0.1:8099/app/myapp/` for multi-app routing.

---

## "Why did my server never become ready?"

**Why:** The harness polls `readiness_port` via TCP connect (not HTTP GET) every 200ms for 30 seconds. If your server does not bind the declared port within 30 seconds, the harness sends `serverFailed`.

**Fix:** Check three things:
- Is the port correct in your app config?
- Is the server binding to `0.0.0.0` (not just `127.0.0.1`)?
- Does it start within 30 seconds?

---

## "Where are my logs?"

**Why:** Log routing depends on how the workload was started.

**Fix:**
- Task mode: logs stream to your terminal via `aegis run`.
- API: `GET /v1/tasks/{id}/logs?follow=true` (ndjson stream).
- Harness captures stdout/stderr line-by-line. Each line becomes a JSON-RPC `log` notification.
- Daemon logs: check the terminal where `aegisd` is running, or the output of `aegis up`.

---

## "Why didn't my secrets appear in the environment?"

**Why:** Secret injection timing depends on the mode.

**Fix:**
- For app serve mode: secrets are resolved at boot time. If you set a secret after starting the app, stop and re-serve: `Ctrl+C`, then `aegis app serve myapp` again.
- For task mode: pass `app_id` in the task request to resolve secrets. Without `app_id`, no secrets are injected.
- Check secrets exist: `aegis secret list myapp`

---

## "How do I reset all Aegis state?"

**Why:** Sometimes you need a clean slate -- corrupted DB, stale images, or a broken master key.

**Fix:**
1. Stop the daemon: `aegis down`
2. Remove all data: `rm -rf ~/.aegis/`
3. This deletes: database, images, releases, workspaces, master key, base rootfs.
4. You will need to `make base-rootfs` again and re-set all secrets.

---

## "aegis doctor says libkrun not found"

**Why:** The libkrun shared library is not installed or not at the expected path.

**Fix:** Install via Homebrew:
```
brew tap slp/krun && brew install libkrun
```
The library must be at `/opt/homebrew/lib/libkrun.dylib` (Apple Silicon) or `/usr/local/lib/libkrun.dylib` (Intel).

---

## "My VM boots but the command fails immediately"

**Why:** Image architecture mismatch or missing binary.

**Fix:**
- Check the image is linux/arm64. Aegis enforces this and will reject amd64 images with "no linux/arm64 variant found".
- Check the binary or script exists in the image. The image provides the filesystem; your command must reference a path that exists in it.

---

## "aegis run hangs / never returns"

**Why:** The task has a 15-minute hard timeout. If your command runs longer, it will be killed and marked TIMED_OUT.

**Fix:**
- For long-running tasks, check daemon logs for errors.
- If the daemon is not running, start it first: `aegis up`.
- If the task legitimately needs more than 15 minutes, this is a platform limit in the current milestone.

---

## "I deleted master.key and now secrets don't work"

**Why:** Expected. The master key encrypts all secrets. Without it, stored secrets are undecryptable. There is no recovery path.

**Fix:**
- Re-set all secrets via `aegis secret set`.
- A new master key is auto-generated on next daemon start.
- The old encrypted values are gone. You must provide the plaintext again.

---

## "Multiple apps but requests go to wrong one"

**Why:** Without a path prefix or header, the router uses a default fallback (first instance in the map). This is non-deterministic with multiple apps.

**Fix:** Use explicit routing:
- Path routing: `curl http://127.0.0.1:8099/app/myapp/`
- Header routing: `curl -H "X-Aegis-App: myapp" http://127.0.0.1:8099/`

---

## "Workspace directory is empty on host"

**Why:** Workspace paths use the app ID, not the app name.

**Fix:**
- Workspaces are at `~/.aegis/data/workspaces/{appID}/`, not `~/.aegis/data/workspaces/{appName}/`.
- Find the appID: `aegis app info myapp` -- look for the ID field.
- Task mode (`aegis run`): no workspace is created unless the task references an app.
