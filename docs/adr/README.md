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
| [0005](0005-static-domain-api-provisioned-tunnel.md) | One static domain, provisioned through the Cloudflare API from Go | Accepted |
| [0006](0006-split-plane-json-control-binary-data.md) | Split plane: JSON control frames, binary data frames | Accepted |
| [0007](0007-server-owned-viewport-modes.md) | Server-owned viewport modes; per-connection LIVE streams | Accepted |
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

## Themes

- **The API is the product** — 0002, 0006, 0007, 0008, 0015, 0016 define a
  contract a client that has never seen this repo can be built against. See
  [`../api.md`](../api.md).
- **Security is a feature, not a checkbox** — 0004, 0009, 0010, 0017. This binary
  executes arbitrary commands.
- **Performance is the differentiator** — 0006, 0014, 0018, 0024. Measured, not
  claimed, and spent on latency rather than paint time.
- **It has to actually start** — 0012, 0019, 0020. Every one of these is a
  failure that surfaces to the user as an unexplained reconnect loop.
- **Verified, not assumed** — 0003, 0007, 0009, 0011, 0021, 0025 exist because
  Herdr 0.9.0 was inspected live (`herdr api schema`, real subcommand and
  `plugin link` runs) and a real implementation was read. Every one of them
  disagreed with the obvious design. The undocumented upstream facts worth
  knowing before writing any Herdr client are in 0021 (one request per socket
  connection), 0011 (`session.snapshot`; three subscriptions need a `pane_id` or
  the whole call fails), 0003 (the `full` flag on `terminal.frame`) and 0025
  (`:` is a legal id character, `.` is not).
