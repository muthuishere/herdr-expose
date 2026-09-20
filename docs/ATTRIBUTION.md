# Attribution

herdr-expose itself is **MIT-licensed** — see [`../LICENSE`](../LICENSE),
© 2026 Muthukumaran Navaneethakrishnan. Everything credited below is also MIT,
so the whole tree is distributable under one permissive licence with no
copyleft obligation anywhere in it.

## herdr-remote

**[github.com/dibin666/herdr-remote](https://github.com/dibin666/herdr-remote)**
— MIT License, © 2026 dibin666.

herdr-remote is a Node implementation of the same idea: reach your Herdr panes
from a phone. It is a real, working product, and reading it made this one
substantially better.

**We did not copy code.** herdr-expose is an independent implementation in Go.
What we took is knowledge, and we took a lot of it:

- **Protocol shape.** The split between a JSON control plane and binary terminal
  frames, and the observation that a negotiated binary frame beats base64 in
  JSON, come from reading their relay. We diverge by making binary mandatory and
  unnegotiated (ADR 0006) — they have an installed base to keep working and we
  do not.
- **Mobile terminal engineering.** Most of what is hard about a terminal on a
  phone, we learned from their scar tissue: probe the renderer after content is
  drawn and degrade to DOM when the canvas comes back blank; never
  `transform: scale()` an xterm surface, because cell hit-testing uses the
  unscaled cell width; use 1.30 rather than the configured `lineHeight` for
  cell-height fallback; drive layout from `visualViewport.height` because
  `100dvh` and `window.innerHeight` both lie when the soft keyboard is up;
  replay taps through xterm's core mouse service instead of synthesising DOM
  mouse events. Every one of those is a day we did not lose.
- **Operational failure modes.** The orphan-holding-the-port → EADDRINUSE retry
  → duplicate-connectors → endless-reconnect chain (ADR 0019), and the minimal
  `$PATH` under launchd/systemd breaking a bare `herdr` exec (ADR 0020), are
  their documented pain.
- **Config path ambiguity.** Honouring `$HERDR_PLUGIN_CONFIG_DIR` gives the tool
  two different configs depending on launch path. They hit it and wrote it down;
  we simply do not do it (ADR 0009).

**Where we deliberately diverge**, and why — none of this is a criticism of
their choices, which are correct for their product:

| | herdr-remote | herdr-expose |
|---|---|---|
| Runtime | Node | Go, single static binary |
| Terminal source | spawns the whole Herdr TUI in a node-pty | `herdr terminal session observe/control` per pane |
| Product | Herdr's own UI, remoted | pane tree, summary tiles, mobile-native layout |
| Remote path | hosted relay | loopback + a tunnel the user starts |
| Framing | negotiated binary v1/v2 with JSON fallback | one mandatory binary framing |

Their MIT licence permits far more than we took. We mention it here because
credit is cheap and accuracy about where ideas came from is worth more than the
appearance of having invented them.

## Herdr

**[herdr.dev](https://herdr.dev)** — the terminal multiplexer this plugin is
built on. herdr-expose is transport and UI; Herdr does the actual work. Verified
against Herdr 0.9.0, protocol 22.

## Licence compatibility, stated plainly

| | Licence | What we owe it |
|---|---|---|
| herdr-expose | MIT, © 2026 Muthukumaran Navaneethakrishnan | — |
| herdr-remote | MIT, © 2026 dibin666 | nothing legally: we ship none of its code. Credited here because accuracy about where ideas came from is worth more than the appearance of having invented them. |
| Herdr | see [herdr.dev](https://herdr.dev) | it is a runtime dependency we invoke, not code we vendor. Install it yourself. |

Go module dependencies keep their own licences. The direct ones are
`dop251/goja` (MIT), `gorilla/websocket` (BSD-3), `pelletier/go-toml/v2` (MIT)
and `skip2/go-qrcode` (MIT); the indirect ones are MIT, BSD-3 or Apache-2.0.
`go list -m all` is the authoritative list — there is no copyleft in the tree,
which is what lets the single static binary be redistributed freely.

The embedded web app's npm dependencies are listed in `web/package.json`.
