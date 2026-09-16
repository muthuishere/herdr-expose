# 3. Terminal streaming goes through the `herdr terminal session` subprocess

Status: Accepted

## Context

The obvious design is "everything over the socket." We checked before building:
`herdr api schema` on Herdr 0.9.0 (protocol 22) lists **102 request methods and
26 events, and not one of them is `terminal.*`**.

Terminal bytes are only reachable through the CLI:

```
herdr terminal session observe|control <target> --cols N --rows M
```

which emits NDJSON on stdout — `{"bytes":"<base64 ANSI>"}` frames and a final
`{"type":"terminal.closed","reason":...}` — and, in `control` mode, accepts
`terminal.input` / `terminal.resize` / `terminal.scroll` / `terminal.release` as
NDJSON on stdin.

This was **verified empirically on this machine, on both 0.8.2 and 0.9.0**, not
inferred from docs and not assumed. It is worth stating because the best-known
implementation of this product does not use it (see Alternatives).

## Decision

Terminal I/O is a managed subprocess, spawned from an **absolute** path to the
`herdr` binary (ADR 0020). Everything else — tree, panes, agents, commands —
stays on the socket. The upstream layer decodes base64 exactly once, at the
process boundary; every layer below it handles raw `[]byte` (ADR 0006).

Each **connection** gets its own subprocess per LIVE target, with its own
geometry (ADR 0007). Herdr's one-controller / many-observers exclusivity maps
straight through: a second client asking for control gets `CONTROL_HELD` and may
retry with an explicit takeover.

## Consequences

- One OS process per (connection × live target). Bounded by the viewport
  scheduler: only the focused pane is LIVE, so it is ~one process per client.
- Process spawn latency on first attach to a pane. Keystroke latency after
  attach is unaffected, which is the metric that matters.
- If Herdr later adds `terminal.*` socket methods, this is the one layer that
  changes, behind an interface. Nothing above `internal/upstream/` knows.

## Alternatives considered

**Spawn the whole Herdr TUI in a PTY and stream its raw ANSI.** This is what the
Node reference (`dibin666/herdr-remote`) does, and it is a coherent choice: you
get real PTY semantics for free, plus Herdr's own UI in the browser, with no
pane-tree modelling at all.

We did not take it because **the pane tree is our product**. Streaming the TUI
forfeits the pane list, the summary tiles, the blocked-agent Q&A view and any
mobile-specific layout — the browser gets a desktop TUI, shrunk. It also costs
one full Herdr client process per browser tab, against one lightweight observer
per live pane here.

Both paths work. They are different products, and this ADR records which one we
are building so the question does not get relitigated every time the reference
implementation is read.
