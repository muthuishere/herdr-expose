# 4. Loopback-only bind; remote access is always a user-started tunnel

Status: Accepted (relaxed by ADR 0022 and ADR 0023 — see Consequences)

## Context

This binary can call `pane.run`, `pane.send_text` and `agent.prompt`. Those
execute arbitrary commands on the user's machine, as the user. **The moment it
is reachable remotely it is a remote code execution endpoint.** Not "handles
sensitive data" — RCE, by design, because that is what a terminal is.

A convenience flag like `bind = "0.0.0.0"` would expose that to every device on
whatever coffee-shop wifi the laptop joined, with one config typo.

## Decision

- The server binds `127.0.0.1` only. `bind` exists in config for clarity but any
  non-loopback value is **refused with an error at startup**, not warned about.
- Remote access is always an explicit, user-started tunnel via an exposure
  adapter (ADR 0005). Exposure defaults to `enabled = false`.
- Auth is mandatory even on loopback: a bearer token, checked **before** the
  WebSocket upgrade, on every `/v1/*` route. Only `/healthz` is unauthenticated.
- Auth is **two secrets** — a server token and per-device tokens issued by
  pairing — stored as SHA-256 hashes in 0600 state, never in config. The full
  mechanism (pairing TTL, sliding device expiry, revocation, constant-time
  compare, origin allowlist) is ADR 0017.
- No token, in any form, is logged, returned by `/v1/config`, or committed
  (ADR 0009).

## Consequences

- Remote use costs one extra command. That is the intended friction.
- A leaked token is bad but not catastrophic while the tunnel is down; there is
  no second path in.
- Tunnel providers terminate TLS, so the tunnel operator is in the trust path.
  The README says so plainly rather than implying end-to-end encryption.
- Auth bugs are treated as the top-severity class in this repo.
- ADR 0017 refines the mechanism; this ADR fixes the posture it has to serve.
- **This ADR has since been relaxed, on purpose.** ADR 0022 adds a `lan` mode
  that binds `0.0.0.0`, and ADR 0023 drops the token requirement on loopback
  while making Origin and Host pinning mandatory. Both are gated by the
  reasoning above rather than in spite of it: the pairing code is shown only on
  the physically present machine, so reaching the port buys an attacker nothing.
  The posture — *this is an RCE surface and auth is the primary feature* — is
  unchanged and is what those ADRs had to argue against.
