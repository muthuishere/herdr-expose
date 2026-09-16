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

The server holds the authoritative *idle* state, which is a fact about the pane.
Each **connection** holds its own unseen set and derives DONE locally from
`idle AND unseen`. A focus event from a client marks seen **for that connection
only**. Seen state is not persisted across reconnects beyond the resume window;
a genuinely new connection starts by treating currently-idle panes as seen so it
does not open with a wall of stale badges.

## Consequences

- Four clients means four seen sets. They are tiny (a set of pane ids).
- "Mark all read" is per-device, which is what users expect from every other
  notification surface they use.
- A client that reconnects with a valid resume seq keeps its seen set; one that
  gets a full `snapshot` rebuilds it. Both paths are defined, so the behaviour is
  never accidental.
- DONE cannot be computed server-side into a single shared tree field. The tree
  carries `idle`; DONE is a client-local derivation. Documented in `docs/api.md`
  so the mobile client does not reinvent it wrongly.
