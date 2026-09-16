# 19. Three-layer supervision with a managed-pid ledger

Status: Accepted (extends ADR 0012)

## Context

ADR 0012 covers the plugin lifecycle: startup hooks are one-shot and
unsupervised, so `daemon` fork-execs `serve` and exits 0. That is enough to make
`herdr plugin install` produce a working UI, and not enough to keep it working.

The failure it does not cover is the one users actually report, and it never
looks like a process problem:

> An orphaned `serve` from a previous session still holds port 21118. A new
> instance starts, fails to bind, retries, and meanwhile both processes are
> connected as duplicate connectors kicking each other off. **The user sees an
> endless reconnect loop in the browser** and has no idea a stale process exists.

Killing the process by hand makes it worse if a service manager is in charge: the
manager respawns it, and a manual start alongside produces a second copy fighting
for the port.

## Decision

Three layers, all of them:

1. **`daemon`** — fork-exec `serve` detached, exit 0 immediately, so the Herdr
   startup hook completes cleanly. `serve` holds an exclusive flock on the
   pidfile and exits quietly if another instance holds it.
2. **A real service unit** — a launchd LaunchAgent with `KeepAlive` on macOS, a
   `systemd --user` unit with `Restart=always` on Linux. `serve` runs in the
   foreground as that unit's main process. This is what gives crash recovery and
   boot persistence, and installing it is a first-class command, not an appendix
   in the README.
3. **An append-only managed-pid ledger plus `reclaimStrays()` on takeover** —
   SIGTERM every recorded pid, **wait for the port to actually be released**,
   then SIGKILL. Every start / stop / restart routes through an "is a manager in
   charge?" check first, so we ask the manager instead of fighting it.

## Consequences

- Three mechanisms to keep consistent, and the ledger must be crash-safe
  (append-only, tolerant of pids that no longer exist or have been reused).
- Waiting for the port rather than for the process is the part that matters:
  process exit does not imply the listener is released.
- "Is a manager in charge?" must be answered per platform (launchd vs systemd vs
  neither). Small, platform-specific, and worth writing once properly.
- A user who wants none of this can still run `serve` in the foreground; layer 1
  keeps that harmless.
