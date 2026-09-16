# 2. The Herdr socket API is the only upstream contract

Status: Accepted

## Context

Herdr owns panes, tabs, workspaces, worktrees, agent detection and session
restore. There are three ways to read that state: the socket API
(`$HERDR_SOCKET_PATH`, 102 request methods and 26 events in 0.9.0), scraping the
TUI, or reading Herdr's on-disk state files.

Two of those three are tempting because they appear to expose more.

## Decision

The socket API — reached directly, or via `$HERDR_BIN_PATH` for one-shot
commands — is the only upstream contract. We never parse the TUI and never read
Herdr's state files. If a fact is not in the socket API, for our purposes it
does not exist; we file it upstream instead of working around it.

Upstream types are generated from `herdr api schema --output schema.json`, not
hand-written, and regenerated on every Herdr bump.

## Consequences

- A Herdr upgrade that changes the schema breaks the build at compile time,
  which is the cheapest possible place to learn about it.
- Some things we want are simply unavailable until upstream adds them. That is a
  feature: it keeps this binary transport + UI and nothing else.
- `herdr_version` and the protocol generation are surfaced in the `welcome`
  frame so a client can see drift rather than guess at it.
- Exactly one exception exists, and it is deliberate and documented: terminal
  streaming (ADR 0003). It uses the official `herdr` CLI, not the TUI or state
  files, so the spirit of this rule holds.
