# 13. Mobile-first: 375px is the design target

Status: Accepted

## Context

The reason this project exists is to check on agents from a phone. The desktop
already has a perfectly good client: the Herdr TUI, on the machine where the
work is happening. Nobody needs a second desktop terminal UI.

Desktop-first web UIs get a mobile pass at the end, and it is always the same
outcome: a horizontally scrolling grid, a terminal you cannot type into, and
controls under the iOS home indicator.

## Decision

**375px is the design target.** Every view must work at 375px before it works at
1440px, and it is tested at 375 first.

- Single-column pane list → tap a pane → full-screen terminal with a fixed key
  bar (y / n / enter / esc / arrows / 1 2 3).
- The desktop grid is a `@media (min-width: 900px)` **enhancement**, not the
  baseline.
- No horizontal page scroll, ever.
- Safe-area insets honoured; the key bar sits above the home indicator.
- The blocked-agent Q&A view is generic — `agent.read --source detection` plus
  the key bar — with no per-agent-kind parsing to maintain.

## Consequences

- Some desktop density is left on the table. The TUI covers that case.
- Every new feature carries a 375px review; "we'll make it responsive later"
  is not available.
- It sets the shape of the eventual native mobile app: the same information
  hierarchy, the same one-pane-at-a-time model, over the same API.
