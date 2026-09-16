# 23. No token on loopback — but Origin and Host pinning are not optional

Status: Accepted

## Context

In `local` mode the server is bound to `127.0.0.1`. Anyone who can reach that
already has shell on the box, so a device token adds no security — it only adds a
pairing step to open your own terminal on your own machine.

**But "no token" must not be read as "no checks."** The real attacker against a
loopback service is not a local user, it is **the browser**. Any website you
visit can issue requests to `127.0.0.1`, and **DNS rebinding** turns a hostile
page into a fully-fledged client of this server. That is an actively exploited
bug class against local dev servers — and this particular local server runs
arbitrary commands.

## Decision

In `local` mode no device token is required. Replacing it, and **mandatory**:

- **Origin allowlist, strictly enforced** on the WebSocket upgrade and on every
  non-GET request. Only `http://127.0.0.1:<port>` and `http://localhost:<port>`
  are accepted. **A missing Origin on a browser-initiated upgrade is rejected**,
  not defaulted to allow.
- **Host header pinning.** Reject any request whose `Host` is not
  `127.0.0.1:<port>` or `localhost:<port>`. This is the control that defeats DNS
  rebinding: the rebound name arrives in `Host` and does not match.
- `SameSite` cookies where cookies exist, and never a reflected CORS origin.

| mode | bind | device token | Origin + Host pinning |
|---|---|---|---|
| local | 127.0.0.1 | not required | **required** |
| lan | 0.0.0.0 | **required** | **required** |
| cloudflare | 127.0.0.1 | **required** | **required** |

**Auth is skipped only because the listener is loopback — never because a
request claims to be local.** Derive it from the listener, never from a header,
and never from `X-Forwarded-For`.

## Consequences

- Opening the local UI is frictionless, which is what makes the plugin feel
  built-in rather than bolted on.
- The Origin and Host checks are now load-bearing security controls rather than
  hygiene. A bug there is a full compromise, so they are unit-tested directly,
  including the missing-Origin and rebound-Host cases.
- A local HTTP client that is not a browser (curl, a script) must set a matching
  `Host`. That is a small, documented cost and it is the same check that stops
  the attack.
- The token check cannot be keyed off "is this address loopback" data from a
  proxy header. Stated here because it is the one shortcut that would quietly
  undo the whole ADR.
