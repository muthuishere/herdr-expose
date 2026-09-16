# 12. Self-daemonizing with a flock'd pidfile

Status: Accepted

## Context

Herdr plugin `[[startup]]` hooks are **one-shot initialization commands, not
supervised services**. Herdr runs the hook once after session restore, when the
API is ready, and does not restart the process if it dies. The hook is also
expected to complete — a startup command that blocks forever is a startup
command that looks hung.

The hook fires on every session restore, so it can and will run while a server
from a previous session is still alive.

## Decision

Two commands, and the manifest's `[[startup]]` uses the first:

- `herdr-expose daemon` — fork-execs `herdr-expose serve` detached, then exits 0
  **immediately**, so the startup hook completes cleanly.
- `herdr-expose serve` — the real server. Takes an **exclusive flock** on the
  pidfile (`$HERDR_PLUGIN_STATE_DIR`, else `$XDG_RUNTIME_DIR`) and exits quietly
  with status 0 if another instance already holds it.

This makes double-start harmless by construction rather than by a PID-liveness
check that races. `status` and `stop` read the same pidfile.

Self-daemonizing is the default because it makes `herdr plugin install` produce
a working UI with zero extra steps. It is **layer 1 of three**: a launchd /
systemd unit and a managed-pid ledger with stray reclamation are required as
well, and are ADR 0019.

## Consequences

- This layer alone gives no crash recovery and does not reclaim an orphan
  holding the port — which users experience as an endless browser reconnect
  loop. That is exactly what ADR 0019 exists to fix.
- flock is held for the process lifetime, so a kill -9 releases it correctly —
  unlike a stale PID file, which is the classic failure this avoids.
- Windows has no flock; it is out of scope (`platforms = ["macos","linux"]`) and
  would need a named mutex.
