# 8. Seen state is per-connection, never global

Status: Accepted

## Context

Herdr's `done` status means *idle but unseen*. `pane focus` and `agent focus`
mark a pane seen; reads do not. Each Herdr TUI client already tracks completions
independently, which is the right behaviour: what I have looked at says nothing
about what you have looked at.

If herdr-expose kept one global seen set, then with web + TUI + two phones
attached, whoever glances at a pane first silently clears the Done badge for
everyone else. The badge that tells you an agent finished while you were away is
exactly the thing you cannot afford to have wiped by another device.

## Decision

The server holds the authoritative *idle* state, which is a fact about the pane,
**and one unseen set per connection**. `done` is derived as `idle AND unseen-by-
this-connection` and shipped in that connection's `tree` — so the server does the
derivation, but separately for every connection, and two clients legitimately
receive different `done` values for the same pane in the same instant.

A focus event from a client marks seen **for that connection only**. A new
connection starts by treating currently-idle panes as seen, so a client does not
open with a wall of stale badges.

## Consequences

- Four clients means four seen sets. They are tiny (a set of pane ids).
- "Mark all read" is per-device, which is what users expect from every other
  notification surface they use.
- A client that reconnects with a valid resume seq keeps its seen set; one that
  gets a full `snapshot` rebuilds it. Both paths are defined, so the behaviour is
  never accidental.
- `done` **cannot** be a single shared field computed once and broadcast. It is
  per-connection state that happens to live on the server, which means the tree
  is rendered per connection rather than serialised once for everybody. That is a
  real cost on the fanout path and it is the price of the feature.
- Clients must **render `done`, not recompute it**, and must not treat it as a
  property of the pane they can cache globally. Documented in `docs/api.md`.
