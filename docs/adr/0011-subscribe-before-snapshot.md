# 11. Subscribe before snapshot

Status: Accepted

## Context

The intuitive bootstrap order is: fetch a snapshot of the tree, then subscribe
to events, then apply events to the snapshot. This is correct only if the event
stream replays retained history from before the subscription.

**Herdr 0.9.0 stopped replaying retained event history on `events.subscribe`.**
Verified against 0.9.0 / protocol 22. With snapshot-then-subscribe, every change
that happens between the snapshot response and the subscription taking effect is
lost — silently, because nothing errors and the tree merely becomes subtly wrong
until the next unrelated resync.

## Decision

Always **subscribe first, then snapshot**. Events that arrive before the
snapshot is applied are buffered and replayed against it afterwards, discarding
any that the snapshot already reflects. The same order applies on every
reconnect to the Herdr socket.

On reconnect we resync the whole tree rather than diffing: it is one request and
it cannot be wrong.

## Consequences

- A short buffer of pre-snapshot events and an idempotent apply path are
  required. Both are small; the bug they prevent is not.
- Events must be idempotent or carry enough identity to dedupe against the
  snapshot.
- This ordering is easy to reverse during a refactor and the failure is silent,
  so it belongs in a comment at the call site and in a test, not only here.
