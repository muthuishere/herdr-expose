# 16. No resume; reconnect repaints

Status: Accepted (supersedes the ring-buffer / seq-replay design)

## Context

The original design gave every target a 256KB ring buffer. A reconnecting client
sent its last `seq` and the server replayed from there; if the ring had rolled
past that point, the client got a `gap` followed by a `snapshot`.

That is a lot of machinery — per-target ring buffers, byte-accounting, replay
ordering against live output, a rolled-past path, and a resume handshake — and
every piece of it is correctness surface we would own forever.

It also solves a problem terminals do not have. A terminal is not a message log.
Once you repaint the current screen, there is no perceivable "missed bytes": the
scrollback you lost is scrollback you were not looking at, and the state you care
about is whatever is on screen now.

## Decision

Resume is deleted. On reconnect a client re-subscribes, receives a fresh
`snapshot` of each target, and repaints. No ring buffer, no replay, no
last-seq handshake.

Kept:

- **`seq`** stays in the binary frame header — for ordering within a connection
  and for debugging. It is not a resume cursor.
- **`gap`** stays as a backpressure signal: under overflow we drop buffered
  output for the affected target, emit `gap` with the byte count, then a
  `snapshot`. Losing output under load is acceptable; lying about it is not.

## Consequences

- A large, subtle subsystem simply does not exist. Fewer allocations on the hot
  path (ADR 0014), no buffer-ownership questions, nothing to test.
- Reconnect is slightly more expensive: a full snapshot per visible target
  instead of a delta. At terminal sizes this is kilobytes and it compresses well.
- Scrollback written while disconnected is lost. Herdr still has it; the user can
  scroll. Documented in `docs/api.md` so no client author waits for a replay that
  is never coming.
- Clients must not persist `seq` across connections or attempt to resume. Stated
  as a client requirement in `docs/api.md`.
