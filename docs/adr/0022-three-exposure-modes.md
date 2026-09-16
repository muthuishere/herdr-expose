# 22. Three exposure modes: local, lan, cloudflare

Status: Accepted (relaxes ADR 0004's loopback-only rule, deliberately)

## Context

ADR 0004 said: bind `127.0.0.1`, always, and reach it remotely only through a
tunnel. That is the right default and it was too absolute.

The concrete case: someone on their own wifi, with no domain, no Cloudflare
account and no `cloudflared` installed, wants their phone to reach their laptop.
Under loopback-only the answer is "install a tunnel client and buy a domain,"
which is absurd for two devices four feet apart. And when `cloudflare = true` is
configured but `cloudflared` is simply not present, failing hard is the worst
possible behaviour — the user gets nothing at all.

## Decision

Three modes, resolved in this order:

| mode | bind | when |
|---|---|---|
| **cloudflare** | `127.0.0.1` | `cloudflare = true`, `domain` set, `cloudflared` resolvable |
| **lan** | `0.0.0.0` | `lan = true`, **or automatically** when cloudflare was requested but `cloudflared` is absent — log loudly, fall back, do not fail |
| **local** | `127.0.0.1` | the default when nothing else is configured |

**`bind` is no longer user-settable.** The mode decides it. The key is kept only
so a manual value can be rejected with a message pointing at `lan`.

LAN mode is acceptable **only** because of how pairing works (ADR 0017): the
pairing code is displayed **only on the physically present machine**, and no HTTP
endpoint ever mints or shows one. So the LAN attack surface is an
unauthenticated `/healthz`, a static SPA, and a hard rate-limited `POST /v1/pair`
that only *accepts* codes. Someone on your wifi can reach the port, complete a
handshake, and get exactly nowhere without looking at your screen.

**There is no "trusted LAN" bypass.** A device token is required in lan and
cloudflare modes without exception (ADR 0023 covers why local mode differs).

Mechanics:

- Resolve the primary non-loopback IPv4 at startup; show it in `status`, in the
  UI, and encode it in the QR. **Re-resolve on SIGHUP and on network change** — a
  DHCP lease change must not leave a stale URL in `status`.
- The QR URL follows the mode: `https://<domain>` / `http://<lan-ip>:<port>` /
  `http://127.0.0.1:<port>`.
- The origin allowlist must accept the LAN origin in lan mode, or the browser
  refuses the WebSocket.
- **Log a one-line warning on every lan-mode start**, naming the bind address.

## Consequences

- We now bind a public interface in one mode. ADR 0004's *reasoning* still holds
  and is the reason this mode is gated behind explicit config, mandatory auth and
  a local-only pairing code.
- **Plain HTTP on a LAN IP is not a secure context**, so there is no service
  worker and no PWA install in lan mode (`localhost` is exempt; a LAN IP is not).
  The phone gets a working web app, not an installable one. This is a property of
  the web platform, stated plainly in `status` and the README, not a bug to chase.
- Three modes means three auth/origin configurations to test. The matrix is small
  and it is exactly where security bugs would live, so it is tested explicitly.
