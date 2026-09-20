# 31. Looking must not touch

Status: Accepted (SPEC AMENDMENTS 14; **withdraws** ADR 0007's "a terminal
without a geometry message does not work", and supersedes ADR 0030's per-pane
defaults)

## Context

The owner, working at his laptop with the web UI open on the same session:
**"you are scrolling actual herdr terminal."** Opening a pane in the browser
moved the pane he was typing in — reflow, redraw, scroll, under his hands.

ADR 0030 fixed this for agent panes by giving them a geometry-free transcript.
It did not fix it in general, because a plain shell still defaulted to a live
attach, and a live attach still declared a size.

So it was measured, on herdr 0.9.0, against a throwaway session with `tput`
inside the pane as ground truth:

| call | pane before | during | after |
|---|---|---|---|
| `terminal session observe <p>` | 120x40 | 120x40 | 120x40 |
| `terminal session observe <p> --cols 100 --rows 60` | 120x40 | 120x40 | 120x40 |
| `terminal session control <p> --cols 100 --rows 60 --takeover` | 120x40 | **100x60** | **100x60** |
| `terminal session control <p> --takeover` (no geometry) | 41x13 | **120x40** | **120x40** |
| `pane.read` / `agent.read` / `session.snapshot` | — | unchanged | unchanged |
| `pane.scroll {offset_from_bottom:20}` | offset 0 | **offset 20** | **offset 20** |

Two findings decided the design. **Observing was already harmless** — the
geometry we passed it was simply ignored. The mutating paths were the **control
upgrade** (which any keystroke triggers) and **`pane.scroll`**.

## Decision

**Viewing a pane from the web must never mutate the owner's local session.**
Only an explicit, informed action may.

1. **Transcript is the default for EVERY pane**, agent or plain shell. A shell
   is output like any other output and reads fine as text; "no agent" was never
   a reason to attach to somebody's terminal. The text/term toggle stays,
   remembered per pane.
2. **Terminal view is opt-in, once per pane per session.** The first tap states
   in one line what it costs and waits. Consent lives in `sessionStorage` —
   exactly the lifetime of "do not nag again for this session". Declining does
   not even record the preference.
3. **Match the pane; never impose on it.** A LIVE attach passes **no**
   `--cols/--rows`. Herdr uses the pane's own size and reports it as the first
   frame's width/height, and the server relays that to the client as a new
   `geometry` control frame (`{target, cols, rows, source: "pane"|"client"}`).
   The client renders **at** that grid and scales its **font** to fit its box.
   The observe->control upgrade reuses the same reported size, so taking control
   is control only.
4. **`resize` survives as the explicit action** ("fit to my window"), is
   reversible with `{"match": true}`, and is the **only** thing in this product
   that changes a pane for everyone attached to it. `welcome` says so on the
   wire: `resize_required: false`, `resize_mutates_pane: true`.
5. **Read paths are read-only, and `seen` stays ours.** Summary polls,
   transcript polls, detection reads, agent reads and snapshots are all
   `pane.read` / `agent.read` / `session.snapshot` — measured non-mutating.
   Nothing calls `pane.focus` or `agent.focus`, and nothing may: Herdr's focus
   marks panes seen and would wipe the owner's own Done badges. Unseen state
   stays per connection in `core.SeenSet` (ADR 0008) and never leaves the
   process. The client clears a badge with the `seen` control frame.
6. **`pane.scroll` is removed from the observer path entirely.** A viewer
   scrolls their own 5000 lines of scrollback; only a controller moves the
   shared viewport.

## Consequences

- ADR 0007's rule that geometry must precede the first frame is **gone**. It was
  what made merely opening a pane declare a size for it.
- Nothing upstream reports a pane's **cols** in the 0.9.0 API — the tab layout's
  `rect.width` is the only width anywhere — which is why an observe attached
  with no geometry, reporting the pane's real size in its first frame, is the
  mechanism rather than a query.
- Found during the same audit and fixed here:
  `events.subscribe [{"type":"pane.agent_status_changed"}]` is refused on 0.9.0
  with `missing field pane_id`, so there is **no session-wide push** for the one
  field the whole UI is about — an agent that finished kept its `working` badge
  until some unrelated structural event fired a resync. The hub now re-reads the
  tree every 1.5s **while at least one client is connected**, and not at all
  otherwise. `session.snapshot` is read-only, and an empty room generates no
  upstream traffic.
- A client author cannot infer any of this, so it is stated on the wire in
  `welcome.geometry` and `welcome.viewport` rather than only in `docs/api.md`.
