# 20. Resolve the `herdr` binary to an absolute path before any spawn

Status: Accepted

## Context

We spawn `herdr` for every terminal stream (ADR 0003). Doing that as a bare
`herdr` and letting the OS search `$PATH` works perfectly in development and
fails in production, because of where production actually starts:

**launchd and `systemd --user` units start with a minimal `$PATH`** — typically
`/usr/bin:/bin:/usr/sbin:/sbin`. That contains **none** of the directories a
user-level install writes to: `~/.local/bin`, `~/.cargo/bin`, `~/bin`,
`/opt/homebrew/bin`, `/usr/local/bin`. So under the supervised path (ADR 0019),
which is the recommended one, `herdr` is not on `$PATH`.

The failure mode is nasty. The exec fails **inside the forked child**, after the
WebSocket is already established, so there is no startup error to see. The stream
dies, the client reconnects, the stream dies again: **the browser shows an
endless reconnect loop** and the logs show nothing that names the cause.

## Decision

Resolve `herdr` to an **absolute path before any spawn**, in this order:

1. `$HERDR_BIN_PATH` — Herdr sets it for plugin commands, and it is authoritative.
2. `$PATH` lookup.
3. Explicit fallbacks: `~/.local/bin`, `~/.cargo/bin`, `~/bin`,
   `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`.

Resolution is **re-verified per session**, not cached for the process lifetime —
an upgrade can move the binary under a long-running daemon. If no candidate
exists and is executable, **fail with a real, specific error** at that point:
what we looked for, where we looked, and what to set. Never retry into a loop.

## Consequences

- The supervised install works out of the box instead of failing invisibly.
- One resolver, used by every spawn site; `$HERDR_BIN_PATH` also keeps us
  portable across Unix sockets and Windows named pipes (ADR 0002).
- The fallback list is a maintenance item as install methods change. Cheap, and
  the alternative is a silent failure users cannot diagnose.
- The error message is part of the feature. "herdr not found on PATH
  (/usr/bin:/bin); tried ~/.local/bin, ... ; set HERDR_BIN_PATH" is the
  difference between a one-minute fix and a bug report about reconnect loops.
