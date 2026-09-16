# 18. Same-tick coalescing, not a 16ms timer

Status: Accepted (supersedes "16ms coalescing")

## Context

Bursty agent output arrives as hundreds of tiny writes. Writing each one to each
client is syscall-bound and pointless — the client paints at most once per frame
anyway. The standard fix is a coalescing timer: buffer for ~16ms, then flush.

The timer is the wrong instrument here. It adds **up to 16ms to every keystroke
echo**, on a path whose stated budget is **under 5ms** (ADR 0014). We would be
shipping a latency regression as a performance feature, and on a tunnel — where
the round trip is already 100ms+ — every avoidable millisecond is one the user
can feel.

## Decision

Coalesce on the tick, never sleep to accumulate:

- On each loop iteration, **drain everything already queued** for a target and
  write it as one frame.
- **Never delay a write to collect more.** If one byte is pending, one byte goes
  out now.
- **64KB hard flush** stays: a drain larger than that is split.
- **Input side identical**: coalesce only what is already pending; never delay a
  keystroke.

This keeps the syscall saving — under load the queue is always non-empty and
drains are large — with **zero added latency**. Under light load, which is
exactly when a keystroke echo happens, batching had nothing to save anyway.

## Consequences

- Idle-to-first-byte latency is bounded by the scheduler, not by a timer.
- Frame sizes become load-adaptive for free: small when quiet, large when busy.
- No timer goroutine per target, no timer drift, no "why is it 16ms slow".
- The latency harness (ADR 0014) must measure the quiet case specifically; a
  benchmark that only measures throughput would have been satisfied by the timer.
