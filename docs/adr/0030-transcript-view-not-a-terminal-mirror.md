# 30. Agent panes get a TRANSCRIPT view, not a terminal mirror

Status: Accepted (SPEC AMENDMENTS 13)

## Context

The first version put an xterm on the phone and mirrored the pane. Looking at a
real Claude Code pane through it, the verdict was: *"not proper, often getting
nonsense, need some better representation."*

Mirroring an agent's TUI grid is the wrong abstraction, and no amount of
polishing the mirror fixes it:

- Agent TUIs paint incrementally near the **bottom** of the grid and repaint on
  `SIGWINCH`. Attaching a browser resized the PTY, so the agent threw away its
  screen and redrew — you saw a fragment in a void, and the history you attached
  in order to read was gone.
- A PTY grid is a fixed cols x rows. A phone is not. Reflowing is **impossible**
  because the server only has the post-layout character grid; the line breaks
  have already happened.
- What a person wants from a phone is the **conversation and the state** — what
  did I ask, what did it say, is it stuck, what do I answer — not a faithful
  reproduction of a 100x30 character matrix.

## Decision

A new viewport mode, `transcript`: reflowed readable text, ANSI stripped
**server-side**, delivered on the **control plane** as a JSON `transcript`
frame, polled at ~1Hz via `pane.read` / `agent.read` and sent **only when the
text actually changed** (per subscriber, so a new subscriber always gets one
immediately — suppression is the optimisation, the first frame is the product).
`source` is `recent_unwrapped` normally and `detection` while the agent is
blocked. It is paired with a prompt box that sends `agent.prompt` and a key bar
(y/n/enter/esc/arrows/1/2/3) when blocked.

**A transcript subscriber declares no geometry.** That is the entire point: the
agent's own screen is never disturbed, no `SIGWINCH`, no redraw, no lost
history. Opening a pane on a phone becomes non-destructive. A `resize` for a
transcript target is dropped, and raw binary input to one is refused with
`input_failed` — use `agent.prompt` / `agent.send_keys`.

Terminal view remains, available on any pane via a toggle in the header,
remembered per pane. It is the right tool for a shell, for a TUI, and for when
you need exactness.

## Consequences

- **Be honest about what a transcript is.** It is a rendering of the agent's
  visible **screen buffer**, not a true conversation log — Herdr exposes the
  screen, not the agent's message history. So do not fake structure we cannot
  know: no invented message boundaries, roles or turns. Show the text the agent
  is displaying, cleanly reflowed, with clear separation between its output and
  our chrome. `is_screen_buffer: true` says so on the wire.
- Two renderers to maintain, and a mode the client can be demoted into.
- The 1Hz poll is `pane.read` / `agent.read`, both measured non-mutating (ADR
  0031), so the cost of a transcript subscriber is one cheap read per second and
  nothing upstream moves.
