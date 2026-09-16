# 7. Server-owned viewport modes; per-connection LIVE streams

Status: Accepted (amends the earlier "one shared stream, hub fans out" draft)

## Context

Twenty panes, each a live terminal stream, will stall a browser tab and pin the
CPU of the machine running Herdr. The naive protocol lets each client say "send
me this pane, live", and with several clients attached the worst-behaved one
sets the load.

An earlier draft solved the cost problem with **one upstream stream per target,
fanned out to all clients**. That is wrong on 0.9.0, and wrong in a way that
users feel immediately: a terminal stream carries geometry. A phone attached at
40×20 sharing a stream with a laptop at 200×50 **resizes the laptop's pane** —
precisely the bug Herdr 0.9.0's #3526 fixed upstream. Reintroducing it in the
transport layer would be embarrassing.

## Decision

A client sends `viewport` to declare **what it is rendering**, per target:
`live | summary | none`. That is a statement of intent, not a request. The
server decides the real mode:

| Mode    | When                   | Cost                                  |
|---------|------------------------|---------------------------------------|
| LIVE    | focused pane           | full stream, same-tick coalescing, 64KB flush |
| SUMMARY | visible but unfocused  | `pane.read --source visible` at 1–2 Hz |
| NONE    | offscreen              | state changes only                     |

**LIVE streams are per connection**, each with its own geometry, set by the
`resize` control frame (floor 20×6, required before the first frame). One
client's window size can never affect another's.

**SUMMARY reads are geometry-free**, so they are deduplicated: one
`pane.read --source visible` per target per tick serves every client that has
that pane in SUMMARY. That is where the fan-out saving actually belongs.

Coalescing is same-tick (ADR 0018), not a timer.

## Consequences

- Cost scales with (clients × live panes), which is ~one live pane per client,
  plus one shared read per visible pane. Acceptable and bounded.
- A misbehaving client still cannot make the server stream twenty live panes to
  it; the ceiling is a server property.
- Clients must handle being told a mode they did not ask for, including being
  demoted from LIVE — the UI renders the mode the server sent.
- SUMMARY tiles are pre-rendered ANSI-to-HTML, never an xterm.js instance per
  tile. That is the specific thing that kills the tab.
- Geometry is now a first-class part of the protocol rather than an implicit
  property of a shared stream, and is documented as such in `docs/api.md`.
