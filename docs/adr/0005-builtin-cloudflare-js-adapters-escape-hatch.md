# 5. Built-in Cloudflare/ngrok; JS adapters as the escape hatch

Status: Accepted (supersedes the earlier "all exposure is a JS adapter" draft)

## Context

Putting a terminal on `herdr.example.com` is the single most common thing a user
will want after installing this. An earlier draft made *every* exposure path a
JS file on an embedded goja runtime — elegant, uniform, and wrong for the common
case: it asks somebody to write JavaScript, and to hand-roll a DNS record, in
order to use the feature they installed the plugin for.

The named-tunnel flow is also not a one-liner. It means creating or reusing a
tunnel, writing a CNAME through the Cloudflare API, running `cloudflared`,
health-checking it, and restarting it when it dies. Every user re-implementing
that in JS will get the supervision part wrong.

## Decision

Cloudflare and ngrok are **first-class, in Go, in-process**. The happy path is
two lines of config and zero JavaScript:

```toml
[expose]
cloudflare = true
domain = "herdr.example.com"   # omit => quick tunnel, random *.trycloudflare.com
```

That alone creates or reuses the named tunnel, writes the DNS CNAME via the
Cloudflare API, runs and supervises `cloudflared`, health-checks it, restarts it
on death, and surfaces the URL in the UI and in `herdr-expose status`.
`ngrok = true` behaves the same way.

The Cloudflare API token is read from `$CLOUDFLARE_ALLPURPOSE_TOKEN` **by name,
at point of use**. It is never stored in config, never written to state, never
logged.

**JS adapters on goja remain** — for tailscale funnel, a corporate reverse
proxy, someone's homelab. They are the extension point, not the happy path. The
host API stays deliberately tiny (`spawn`, `onLine`, `setUrl`, `url`,
`localUrl`, `isAlive`, `kill`, `log`, `config`, `env(name)`), with no filesystem
and no network from JS; a crash in an adapter never takes the server down, and
`stop()` must be idempotent. `template.js` is shipped to copy.

## Consequences

- Two providers' quirks now live in Go and must be maintained by us. That is the
  right place for the ones almost everyone uses.
- Supervision, health-check and restart semantics are uniform and tested once,
  instead of being re-derived per adapter.
- The extension point still exists, so an unusual setup is never blocked on us
  shipping a release.
- goja stays in the binary for a path most users never touch. Accepted: it is
  small, and it is what keeps "no node at runtime" (ADR 0001) true.

## Alternatives considered

The Node reference implementation (`herdr-remote`) exposes through a **hosted
relay** that hosts and clients both dial. That removes the tunnel problem
entirely and is a reasonable design. We did not take it because it puts a third
party permanently in the path of an RCE surface (ADR 0004) and turns a
self-hosted tool into a service with uptime. A tunnel the user starts, and can
stop, keeps the trust boundary where the user can see it.
