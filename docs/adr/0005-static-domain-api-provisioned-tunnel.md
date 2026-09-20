# 5. One static domain, provisioned through the Cloudflare API from Go

Status: Accepted for the permanent deployment; its ban on ephemeral tunnels is
narrowed by 0027/0028, and its `ngrok = true` clause is **removed** by 0034
(supersedes both the "all exposure is a JS adapter" draft and the "quick tunnel
by default" draft)

## Context

Two earlier drafts were wrong in opposite directions.

The first made *every* exposure path a JS file on an embedded goja runtime. That
asks somebody to write JavaScript, and hand-roll a DNS record, to use the feature
they installed the plugin for.

The second kept a Cloudflare **quick tunnel** as the zero-setup default: a random
`*.trycloudflare.com` hostname, no account needed. It reads as the friendly
option and it is actively harmful for this product. **A hostname that changes on
every restart breaks PWA installs, breaks bookmarks, and breaks origin-bound
device tokens.** The mobile app is the point; an ephemeral origin makes the
mobile app unusable.

There is also a trap in the standard Cloudflare flow: `cloudflared tunnel login`
opens a **browser** and writes an interactive `cert.pem`. That is not something a
headless plugin on a server can do, and it makes the setup irreproducible.

## Decision

**There is no ephemeral tunnel. At all.** `cloudflare = true` **requires**
`domain`; without it, startup fails with an error telling the user to set one.
The quick-tunnel path is deleted, not deprecated — not even as a fallback.

```toml
[expose]
cloudflare  = true
domain      = "herdr.example.com"   # REQUIRED
tunnel_name = "herdr-expose"        # optional
```

**We provision it ourselves, in Go, over the Cloudflare API — never
`cloudflared login`.** `expose start` is idempotent and does exactly this:

1. **Zone** — `GET /zones?name=<apex>` for the `zone_id`.
2. **Tunnel** — reuse by name if it exists, else create with a 32-byte
   `tunnel_secret` and `config_src: "local"`.
3. **Credentials file** — we write the file `cloudflared login` would have
   produced (`AccountTag` / `TunnelID` / `TunnelSecret`), 0600, in the state dir.
4. **DNS** — upsert a proxied CNAME to `<tunnel-id>.cfargotunnel.com`. **Never
   delete a record we did not create.**
5. **Ingress** — generate `config.yml` pointing the hostname at
   `http://127.0.0.1:<port>`.
6. **Run** — `cloudflared tunnel --config <path> run <id>`, resolved to an
   absolute binary path first (ADR 0020).
7. **Verify** — poll `https://<domain>/healthz` until 200. **A process that
   started is not a tunnel that works**; report the URL only once it answers.
8. **Supervise** — restart on crash with backoff, surface state in `status`.

**Preflight before touching anything**: `GET /user/tokens/verify`, then confirm
zone read and DNS edit on the target zone, and fail with a precise error naming
the missing permission. Never half-provision — a failed DNS step must not leave a
dangling tunnel.

`expose stop` kills `cloudflared` and **leaves the tunnel and DNS in place** —
the domain is static, and tearing DNS down on every stop is the ephemeral
behaviour we just deleted. `expose destroy` is the separate, explicit verb that
removes them, and only ever touches records it created.

The API token is read from `$CLOUDFLARE_ALLPURPOSE_TOKEN` **by name, at point of
use**. Never in config, never in state, never in a log.

> **NARROWED by [ADR 0034](0034-cloudflare-is-the-only-built-in-provider.md).**
> This paragraph originally read: "`ngrok = true` follows the same rule: a
> reserved domain is required, no random URLs." The built-in ngrok provider was
> removed as **unverified surface** — there was no ngrok binary and no token on
> the machine it was written on, so it was unit-tested and never once exercised
> end to end. The static-domain rule above stands for Cloudflare, unchanged.

**JS adapters on goja remain** for everything that is not built in — ngrok,
tailscale, a corporate proxy, a homelab. Since 0034 that is the *only* answer
for those, which makes the adapter the extension point rather than a footnote.

## Consequences

- Setup now requires a domain and an API token. That is a real barrier, and it
  buys a URL you can bookmark, install as a PWA, and bind a device token to.
- We own a slice of the Cloudflare API surface, including its error shapes. Worth
  it: the alternative is every user owning it in JavaScript.
- Reproducible and headless — no browser in the provisioning path — which is what
  makes this work on a remote box over SSH.
- Users who genuinely want a throwaway URL are not served. Deliberate.

## Alternatives considered

The Node reference (`dibin666/herdr-remote`) uses a **hosted relay** that hosts
and clients both dial, which removes tunnelling entirely. Reasonable, and we
declined it: it puts a third party permanently in the path of an RCE surface and
turns a self-hosted tool into a service with uptime.
