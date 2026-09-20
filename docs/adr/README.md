# Architecture Decision Records

Each ADR records one decision, why it was made, and what it costs. Format:
Context / Decision / Consequences / Status. A decision that changes gets a new
ADR, or an explicit "supersedes" note, rather than a quiet edit.

`../../SPEC.md` is the binding build contract and wins over anything here.
`../../references/architecture.md` is background rationale that predates the
Herdr 0.9.0 verification.

| # | Title | Status |
|---|---|---|
| [0001](0001-single-binary-embedded-web.md) | One Go binary with the React app embedded | Accepted |
| [0002](0002-socket-api-is-the-only-contract.md) | The Herdr socket API is the only upstream contract | Accepted |
| [0003](0003-terminal-streaming-via-subprocess.md) | Terminal streaming goes through the `herdr terminal session` subprocess | Accepted |
| [0004](0004-loopback-only-auth-is-the-product.md) | Loopback-only bind; auth is the primary feature | Accepted, relaxed by 0022/0023 |
| [0005](0005-static-domain-api-provisioned-tunnel.md) | One static domain, provisioned through the Cloudflare API from Go | Accepted for the permanent deployment; its ban on ephemeral tunnels is narrowed by 0027/0028, its `ngrok` clause **removed** by 0034 |
| [0006](0006-split-plane-json-control-binary-data.md) | Split plane: JSON control frames, binary data frames | Accepted |
| [0007](0007-server-owned-viewport-modes.md) | Server-owned viewport modes; per-connection LIVE streams | Accepted; its "geometry before the first frame" rule is **withdrawn** by 0031 |
| [0008](0008-per-connection-seen-state.md) | Seen state is per-connection, never global | Accepted |
| [0009](0009-toml-config-outside-repo.md) | One config path, TOML outside the repo, forward-compatible | Accepted |
| [0010](0010-pwa-shell-only.md) | PWA is an installable shell only; no offline terminal | Accepted |
| [0011](0011-subscribe-before-snapshot.md) | Subscribe before snapshot | Accepted |
| [0012](0012-self-daemonizing-flock-pidfile.md) | Self-daemonizing with a flock'd pidfile | Accepted |
| [0013](0013-mobile-first-375px.md) | Mobile-first: 375px is the design target | Accepted |
| [0014](0014-performance-budget-as-acceptance-criteria.md) | The performance budget is acceptance criteria | Accepted |
| [0015](0015-api-is-the-product.md) | The API is the product; the web UI is its first client | Accepted |
| [0016](0016-no-resume-reconnect-repaints.md) | No resume; reconnect repaints | Accepted |
| [0017](0017-hashed-per-device-tokens.md) | Auth: two secrets, hashed per-device tokens | Accepted |
| [0018](0018-same-tick-coalescing.md) | Same-tick coalescing, not a 16ms timer | Accepted |
| [0019](0019-three-layer-supervision.md) | Three-layer supervision with a managed-pid ledger | Accepted |
| [0020](0020-absolute-path-herdr-binary.md) | Resolve the `herdr` binary to an absolute path before any spawn | Accepted |
| [0021](0021-one-socket-connection-per-rpc.md) | One Herdr socket connection per RPC | Accepted |
| [0022](0022-three-exposure-modes.md) | Three exposure modes: local, lan, cloudflare | Accepted |
| [0023](0023-loopback-no-token-but-origin-host-pinning.md) | No token on loopback, but Origin and Host pinning are mandatory | Accepted |
| [0024](0024-predictive-echo-in-v1.md) | Predictive echo ships in v1 | Accepted |
| [0025](0025-hex-namespaced-entrypoint-ids.md) | All plugin entrypoint ids are namespaced `hex:` | Accepted |
| [0026](0026-scoped-time-boxed-shares.md) | A share is a scoped, time-boxed, self-destructing second instance | Accepted |
| [0027](0027-ephemeral-dns-for-shares-only.md) | Ephemeral DNS is legal — for shares only | Accepted |
| [0028](0028-quick-tunnels-for-shares.md) | Quick tunnels come back — for shares only | Accepted; its provider-parity clause narrowed by 0034 |
| [0029](0029-exposure-ladder-climbed-never-guessed.md) | The exposure ladder is climbed, never guessed | Accepted |
| [0030](0030-transcript-view-not-a-terminal-mirror.md) | Agent panes get a TRANSCRIPT view, not a terminal mirror | Accepted; its per-pane defaults superseded by 0031 |
| [0031](0031-looking-must-not-touch.md) | Looking must not touch | Accepted |
| [0032](0032-local-rotating-log-no-otel.md) | A local rotating log file, and no OpenTelemetry | Accepted |
| [0033](0033-self-documenting-config.md) | The config file documents itself | Accepted |
| [0034](0034-cloudflare-is-the-only-built-in-provider.md) | Cloudflare is the only built-in provider; everything else is an adapter | Accepted; **removes** 0005's `ngrok` clause and narrows 0028's provider parity |
| [0035](0035-all-sessions-share.md) | `share --all`: an all-sessions share, confirmed out loud | Accepted; extends 0026's scoped share with the absence of a scope, and leaves 0029's ladder untouched |

## Supersessions

| ADR | Supersedes / amends |
|---|---|
| 0005 | the "all exposure is a JS adapter" draft, **and** the quick-tunnel default |
| 0006 | the "JSON over WebSocket for v1" draft, and protobuf-everywhere before it |
| 0007 | "one shared upstream stream per target, hub fans out" |
| 0009 | `$HERDR_PLUGIN_CONFIG_DIR`-first path resolution |
| 0016 | per-target ring buffers and seq-based replay |
| 0017 | a single long-lived plaintext token in `config.toml` |
| 0018 | "16ms coalescing" |
| 0019 | extends 0012 (`daemon` alone is not supervision) |
| 0022 | relaxes 0004's loopback-only rule; `bind` is no longer user-settable |
| 0023 | relaxes 0004/0017 for loopback only, replacing the token with Origin + Host pinning |
| 0024 | promotes predictive echo from phase 2 to v1 |
| 0027 | the ONE exception to 0005: a share's DNS record is created and destroyed |
| 0028 | narrows 0005's blanket ban on quick tunnels to the permanent deployment only |
| 0029 | **withdraws the auto-escalation** 0028 allowed, and 0022's automatic promotion for shares |
| 0030 | the "one xterm mirror for every pane" product |
| 0031 | **withdraws** 0007's "a terminal without a geometry message does not work"; supersedes 0030's per-pane defaults; `pane.scroll` leaves the observer path |
| 0032 | withdraws the OTEL exporter specified alongside 0033's config work |
| 0033 | extends 0009 (where the config lives) with what a first run writes |
| 0034 | **removes** 0005's `ngrok = true` clause and narrows 0028's provider parity; promotes the JS adapter from escape hatch to the extension point |
| 0035 | extends the scoped share with `--all` (no scope at all); changes nothing about the rung ladder, pairing or time-boxing |

## Themes

- **The API is the product** — 0002, 0006, 0007, 0008, 0015, 0016 define a
  contract a client that has never seen this repo can be built against. See
  [`../api.md`](../api.md).
- **Security is a feature, not a checkbox** — 0004, 0009, 0010, 0017, 0026,
  0029, 0032, 0034. This binary executes arbitrary commands, so the questions
  that matter are *who can reach it* (0029's ladder), *what can they reach*
  (0026's server-side scope), *for how long* (0026's three-way expiry) and
  *what gets written down* (0032's redaction). 0034 adds the one about the
  surface itself: code that has never actually run does not get to sit on it.
- **Looking must not touch** — 0030, 0031, 0008. Viewing a pane from a phone
  must not move the pane somebody is typing in. This was got wrong first, then
  measured, then fixed; 0031 has the measurements.
- **Performance is the differentiator** — 0006, 0014, 0018, 0024. Measured, not
  claimed, and spent on latency rather than paint time.
- **It has to actually start** — 0012, 0019, 0020. Every one of these is a
  failure that surfaces to the user as an unexplained reconnect loop.
- **Verified, not assumed** — 0003, 0007, 0009, 0011, 0021, 0025, 0031 exist because
  Herdr 0.9.0 was inspected live (`herdr api schema`, real subcommand and
  `plugin link` runs) and a real implementation was read. Every one of them
  disagreed with the obvious design. The undocumented upstream facts worth
  knowing before writing any Herdr client are in 0021 (one request per socket
  connection), 0011 (`session.snapshot`; three subscriptions need a `pane_id` or
  the whole call fails), 0003 (the `full` flag on `terminal.frame`) and 0025
  (`:` is a legal id character, `.` is not). 0031 adds the one nobody would
  guess: `terminal session observe` **ignores** the `--cols/--rows` you pass it,
  so observing was always harmless — it was the control upgrade and
  `pane.scroll` that moved the owner's pane.

## Troubleshooting

When one of these decisions surfaces as a confusing symptom — a working tunnel
that looks dead, a quick hostname that stopped accepting a paired device, no
install prompt on a LAN IP — see [`../troubleshooting.md`](../troubleshooting.md).
