# 17. Auth: two secrets, hashed per-device tokens, never a config-file token

Status: Accepted (supersedes the single long-lived token in `config.toml`)

## Context

ADR 0004 establishes what this is: a binary that runs `pane.run`,
`pane.send_text` and `agent.prompt`, i.e. **arbitrary command execution**, made
internet-reachable the moment a tunnel is up.

The original scheme — one long-lived plaintext token in `config.toml` — fails
that threat model in several ordinary ways. It is readable by anything that can
read a file in `~/.config`. It appears in shell history and screenshots. It
cannot be revoked for one device without cutting off all of them. And a 60-second
pairing TTL, as originally specified, is actively hostile on a phone: unlock,
find the app, scan, type — 60 seconds is a failure loop that teaches users to
turn auth off.

## Decision

Two secrets, separate concerns:

- **Server token** — may you talk to this instance at all.
- **Per-device tokens** — issued by pairing, one per device.

Mechanics, all non-negotiable:

- **Store SHA-256 hashes only**, never plaintext, in **state** (0600) — never in
  `config.toml` (ADR 0009).
- **Pairing**: 6-character code, **TTL 10 minutes**, single use, rate-limited per
  address.
- **Device token**: 32 random bytes, **sliding 30-day TTL**, max 32 devices with
  LRU eviction, individually revocable, listable by `herdr-expose status` showing
  user-agent and last-seen IP — **never the hash**.
- **The pairing code is shown ONLY on the physically present machine** — the
  terminal, or the `hex:pair-qr` overlay pane. **No HTTP endpoint ever mints or
  displays one.** `POST /v1/pair` only *accepts* codes. This single property is
  what makes `lan` mode defensible (ADR 0022).
- **`crypto/subtle.ConstantTimeCompare` for every comparison. No exceptions.**
- **Origin allowlist checked *before* echoing any CORS header.** Reflecting an
  origin and then deciding is the standard way to hand a cross-site attacker a
  shell.
- `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`.
- Handshake rate limiting on the WebSocket endpoint; auth checked **before** the
  upgrade.
- A device token is required in **lan** and **cloudflare** modes without
  exception. In **local** mode it is not, and Origin + Host pinning take its
  place — see ADR 0023, which is the argument for why that is safe and what it
  costs.

## Consequences

- Losing a phone is "revoke that device", not "rotate everything".
- A stolen state file yields hashes, not tokens.
- More state to manage: an eviction policy, a sliding-expiry writer, and a
  revocation list. Small, and every line of it is load-bearing.
- 10-minute pairing codes are a deliberate, documented relaxation of a
  security parameter in favour of a flow users will actually complete. Codes are
  single-use and rate-limited, which is what carries the weight.
