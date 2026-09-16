# 14. The performance budget is acceptance criteria, not aspiration

Status: Accepted

## Context

"Fast" is the differentiator of this product against a functionally similar Node
implementation. A differentiator that is asserted rather than measured decays
silently: someone adds a convenient `[]byte` copy per client, someone marshals a
struct on the fanout path, and six weeks later the thing is merely as fast as
what it replaced, with nobody able to point at the commit.

Terminal latency is also uniquely noticeable. A 40ms keystroke delay is
invisible in a web app and unusable in a shell.

## Decision

These are acceptance criteria. A change that breaks one is a regression, not a
trade-off to be discussed later:

| Path                           | Target                            |
|--------------------------------|-----------------------------------|
| Keystroke → upstream write     | < 5ms                             |
| Output → LIVE client (LAN)     | < 20ms including coalescing       |
| Allocations per terminal frame | **zero** steady-state             |
| Idle CPU, 20 panes             | < 1%                              |
| RSS, 20 panes                  | < 60MB                            |

Non-negotiable implications:

- **Pooled buffers** (`sync.Pool`) on the fanout path. Steady-state streaming
  allocates nothing per frame.
- **LIVE streams are per connection**, each with its own geometry (ADR 0007) —
  sharing one stream across clients would let a phone resize a laptop. The
  dedup that is safe, and required, is on geometry-free SUMMARY reads: one per
  target per tick, however many clients are watching.
- **Never a per-client copy** of a frame that could be shared. The binary
  framing (ADR 0006) exists so one encoded buffer serves every client.
- **Same-tick coalescing** (ADR 0018), never a timer that would spend up to
  16ms of the 5ms keystroke budget. 64KB hard flush.

**We ship a latency harness that measures these**, and it is run before a
release. A claimed number is not a number.

## Consequences

- The fanout path is written with an allocation budget in mind and reviewed that
  way; `go test -bench` with `-benchmem` is part of the definition of done.
- Pooled buffers mean ownership rules: a frame buffer is read-only once
  published and returned exactly once. This is the main source of subtle bugs in
  this design and is worth a race-detector test.
- Some abstractions are unavailable on the hot path. They remain fine everywhere
  else; this budget applies to streaming, not to `/v1/config`.
- The harness is a shipped artifact, so a user reporting "it feels slow" can run
  it and send us a number.
