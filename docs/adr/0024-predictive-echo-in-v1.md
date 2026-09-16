# 24. Predictive echo ships in v1

Status: Accepted (promotes what was scheduled as phase 2)

## Context

The instinct for "make it fast" is to optimise the renderer. The numbers say
otherwise: **paint time is about 1ms; tunnel RTT is 30–150ms.** Every millisecond
we could win on the client is invisible next to the network, and a keystroke that
appears 120ms after you press the key feels broken no matter how cleanly it is
drawn.

Local echo is the only thing that removes that delay from perception. Mosh proved
it a decade ago over far worse links.

The reason it is usually deferred is the failure mode: a naive local echo shows
characters that turn out to be wrong — in a password prompt, in a TUI, in a
vim command — and a phantom character in a terminal destroys trust instantly.

## Decision

Predictive echo is **v1, not phase 2**. Mosh-style, with the safety property
first:

- Predictions start **tentative and invisible**. They are recorded, not drawn.
- A prediction becomes **confident and renders** only after one has been
  confirmed by real server output.
- **Any mismatch wipes all pending predictions** and drops back to tentative.

So a phantom character is never shown: the worst case is that echo silently falls
back to the true round trip, which is exactly where we would have been without
it.

Complementary, and kept as built:

- **Renderer order: WebGL where proven, Canvas where WebGL is risky, DOM as last
  resort**, with the probe-and-degrade machinery intact. Do **not** force WebGL
  on mobile — context loss there renders a blank black rectangle, and canvas
  already paints far faster than the network delivers. Trading a correctness
  cliff for an imperceptible gain is a bad trade.
- `cursorBlink: false` — blinking repaints ~2×/second forever on an idle
  terminal. The biggest free win available.
- `smoothScrollDuration: 0`; never set `minimumContrastRatio` (a per-cell colour
  computation every paint, and it also breaks colour fidelity).
- Await `document.fonts.ready` **before** constructing the terminal, or cell
  metrics are measured against a fallback font and the grid reflows on swap.
- Feed `Uint8Array` to `write()`, never a string — no UTF-8 round trip.

**Measure, do not assert**: the harness reports keystroke-to-echo latency at p50
and p95 **in all three exposure modes**.

## Consequences

- Real complexity in the client: a prediction queue, a confirmation matcher, and
  a wipe path. It is the single largest perceived-speed win available and nothing
  else comes close.
- The matcher must be conservative. When in doubt, wipe — a slightly less
  effective prediction is free, a wrong character is not.
- This constrains the protocol: input is raw bytes the client can model locally
  (ADR 0006), which is another reason there is no JSON encode on that path.
