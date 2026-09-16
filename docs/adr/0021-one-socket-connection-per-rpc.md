# 21. One Herdr socket connection per RPC

Status: Accepted

## Context

**The Herdr 0.9.0 socket serves exactly one request per connection, then closes
it.** Pipelining a second request down the same connection gets a broken pipe.

This is undocumented upstream, and it is the kind of thing that silently breaks
any client written the obvious way. Every competent engineer's instinct on a unix
socket is to dial once, keep the connection, and multiplex — that instinct
produces a client that works for exactly one call and then fails in a way that
looks like a Herdr crash rather than a protocol misuse.

The one exception is `events.subscribe`, which is a stream: that connection is
held open for the life of the subscription.

## Decision

- **Every RPC dials a fresh unix connection**, sends one request, reads one
  response, closes. No connection pool, no pipelining, no keepalive.
- **Only `events.subscribe` holds a connection open**, and it is the only one.
- The RPC helper enforces this by construction — there is no API in
  `internal/upstream` that lets a caller send twice on one connection.
- This is written down here, in `docs/api.md`, and in a comment at the dial site,
  because the failure mode (broken pipe on the *second* call) does not point at
  its own cause.

## Consequences

- A connect + close per call. On a unix domain socket this is microseconds, and
  it is dwarfed by everything else we do. Measured, not assumed.
- Terminal streaming does not go through this path at all — it is a subprocess
  (ADR 0003) — so the hot path is unaffected.
- If upstream later supports pipelining or keepalive, this is a one-function
  change and a win we can take deliberately. Until then, the safe thing is also
  the simple thing.
- Anyone building a different client against the Herdr socket needs this fact.
  It is called out prominently in `docs/api.md` for that reason, even though it
  is technically upstream's contract and not ours.
