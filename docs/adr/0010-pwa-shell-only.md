# 10. PWA is an installable shell only; no offline terminal

Status: Accepted

## Context

An installable PWA is worth a lot on phones: a home-screen icon, standalone
display without browser chrome, correct safe-area handling. It is also the
cheapest possible path to "a mobile app" before a native one exists.

Service workers invite a second idea: cache things for offline use. A terminal
is inherently online. A cached pane is a screenshot of the past presented as
live state — and for a surface that executes commands, a stale view is not a
degraded experience, it is a dangerous one.

## Decision

`vite-plugin-pwa`, installable, `display: standalone`, maskable icon,
safe-area insets honoured. The service worker precaches **only the app shell**:
HTML, JS, CSS, icons — enough to boot and render a clear "disconnected" state.

`/v1/*` is **never** cached: not the stream, not `/v1/config`, not `/v1/pair`.
No background sync, no offline queue of keystrokes.

## Consequences

- Opening the app offline shows the shell and an honest disconnected screen
  rather than a stale terminal.
- Keystrokes typed while disconnected are lost, not queued and replayed later
  into whatever is on screen when the link returns. That is the safe failure.
- The service worker must not intercept the WebSocket upgrade or any `/v1`
  fetch; this is an easy regression and is worth a test.
- `index.html` and `sw.js` are served `no-cache` so a new binary's UI is picked
  up on the next load instead of being pinned by the old worker.
- **In `lan` mode there is no PWA at all.** Service workers and installation
  require a secure context; `localhost` is exempt but a plain-HTTP LAN IP is not,
  so `status.secure_context` is `false` and the phone gets a working web app
  rather than an installable one (ADR 0022). That is a property of the web
  platform, not a bug to chase — the app must detect it and not offer an install
  prompt that cannot work.
