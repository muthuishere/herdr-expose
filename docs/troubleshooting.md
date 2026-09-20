# Troubleshooting

Every entry here is a failure that actually happened while building this, not a
hypothetical. Each one is the same shape: **a confusing symptom, a boring
cause**, and what `herdr-expose doctor` tells you about it.

Start with:

```bash
herdr-expose doctor          # pass/fail per dependency, with a hint on each failure
herdr-expose status          # mode, bind, url, tunnel, devices, sessions
herdr-expose logs -n 100     # the ONLY way to see what a detached daemon did
```

`doctor` checks, in order: `config`, `state dir`, `herdr binary`,
`herdr sessions`, `herdr socket`, `port`, `service manager`, `exposure`,
`cloudflared`, `cloudflare token`, `dns`, `shares`, `log`. Read the check name
first; the hint under a failure names the fix.

---

## The tunnel works from every other device, but this machine says it is dead

**Symptom.** `dig` resolves the hostname. The tunnel is registered, Cloudflare
shows it healthy, a phone on cellular loads the page — and `herdr-expose expose
start` sits there waiting, or `doctor` reports `dns: <domain> does not resolve`.

**Cause: a cached NXDOMAIN in the local stub resolver.** We create a hostname
and then probe it seconds later. Cloudflare publishes it almost immediately, but
a stub resolver that was asked for that name *before it existed* can hold the
negative answer well past the record's TTL. macOS `mDNSResponder` does exactly
this. Nothing is wrong with the tunnel; the machine that created it is the one
machine that cannot see it.

**How the tool already works around it.** The health probe
(`internal/expose/status.go`) deliberately resolves through **public DNS**
(`1.1.1.1`, falling back to `8.8.8.8`) instead of the host resolver, precisely so
a stale negative cache cannot make a working tunnel look dead.

**What `doctor` says.** Note the asymmetry: `doctor`'s `dns` check uses the
**host** resolver on purpose, so it reports what *your browser* will experience.

- `dns: FAIL — <domain> does not resolve` **while** `exposure: PASS` and the URL
  answers from another device ⇒ this is the stale cache, not a broken tunnel.
- `dns: PASS` ⇒ resolution is fine and any failure is elsewhere.

**Fix.**

```bash
sudo dscacheutil -flushcache && sudo killall -HUP mDNSResponder   # macOS
sudo resolvectl flush-caches                                      # systemd-resolved
```

Then re-check from outside the machine before believing anything:

```bash
curl -s https://<domain>/healthz            # expect {"ok":true,"api":"2"}
dig +short @1.1.1.1 <domain>                # bypasses the stub resolver
```

**Related trap:** a process that started is not a tunnel that works. The
Cloudflare edge answers `502` and `530` with a perfectly good HTTP response
while nothing is routing, which is why only a `200` on `/healthz` counts as up.
If you see `530` on a hostname that used to work, the tunnel behind it is gone —
that is a dead DNS record, not a DNS problem.

---

## A quick share's URL worked yesterday, and the phone now gets 401

**Symptom.** A paired phone opens a `*.trycloudflare.com` URL and is asked to
pair again, or is simply rejected. Nothing was revoked.

**Cause: the hostname is new.** A quick tunnel's hostname is assigned by the
edge at start and is **different every time** — including after a restart within
the same share. Device tokens are **origin-bound**, so a token issued against
`mid-ancient-stuff-tokyo.trycloudflare.com` means nothing to
`brief-copper-orbit-lima.trycloudflare.com`. This is a property of the transport
(ADR 0028), not a bug, and the CLI prints it once when you create a quick share.

**Fix.** Re-pair at the current address. Do not resend a hostname you read five
minutes ago.

```bash
herdr-expose share list --json          # re-reads the LIVE url
herdr-expose share pair <id> --json     # a fresh single-use code at the current URL
```

**If you need a link that survives** — a daily driver, a bookmark, a PWA install
— use `--domain X` (a hostname in a zone your Cloudflare token reaches), or the
permanent deployment, which requires a static domain for exactly this reason.

**The LAN version of the same problem:** a `--lan` share's IP is a DHCP lease and
can move, which silently invalidates a printed QR. `share list` re-resolves and
`share pair <id>` re-issues at the current address. The daemon re-resolves its
LAN IP on `SIGHUP` and every 30 seconds, so `status` is never stale for long.

---

## The phone can use it, but there is no "Add to Home Screen"

**Symptom.** On a LAN URL (`http://192.168.1.24:21118`) the web app works
perfectly, but there is no install prompt and the service worker never
registers. On `http://localhost:21118` the same build installs fine.

**Cause: a LAN IP is not a secure context.** Browsers require a secure context
for service workers and installability. `localhost` is explicitly **exempt**; a
plain-HTTP LAN IP is **not**. Nothing in this tool can change that.

**What `doctor` says.**

```
exposure   PASS   mode lan, binds 0.0.0.0:21118, url http://192.168.1.24:21118
           hint:  LAN mode: anyone on this network can reach the port. Auth is
                  still required, and plain HTTP on a LAN IP is not a secure
                  context — the web app works but the PWA cannot be installed
```

`herdr-expose status` prints the same thing as a `note` line, and
`share list --json` / `expose status` carry `secure_context: false`.

**Fix — pick the trade-off deliberately.**

| want | do | cost |
|---|---|---|
| a working web app on the wifi, now | nothing; this is working as designed | no install, no offline shell |
| an installable app | `--quick` (https, but a new hostname each time — install will break) | throwaway only |
| an installable app that stays installed | `--domain X`, or the permanent deployment | needs a zone your Cloudflare token reaches |

A native mobile client is unaffected by any of this — it talks to the same API
(`docs/api.md`) and does not care about browser secure-context rules.

---

## `panic` or `revoke` exits non-zero and says something is still up

**Treat a non-zero exit as "still exposed."** Both commands report per component
— `process_gone`, `dns_gone`, `tunnel_gone`, `port_free`, `state_wiped` — and
deliberately fail loudly rather than claiming success. A kill switch that
silently half-works is worse than none.

Two of these look like failures and are not:

**A revoked share sitting in `orphaned` for ~60 seconds.** Cloudflare refuses to
delete a tunnel until its edge connections are marked inactive, which is roughly
a minute after `cloudflared` dies. Teardown retries and deliberately **keeps**
`share.json` (credentials wiped immediately, state `orphaned`) so the next sweep
can finish the job — deleting the paperwork would strand the tunnel with nothing
left to clean it up. Re-check rather than re-running:

```bash
herdr-expose share list --json     # expect [] once the sweep completes
```

**`cloudflared STILL RUNNING` after `panic`.** `panic` raises a halt flag
*before* killing anything, because the daemon's supervisor would otherwise
respawn `cloudflared` a second later. If the running daemon is an older build
that does not poll that flag, the respawn wins. Restart the daemon on the
current binary and run `panic` again:

```bash
herdr-expose stop && herdr-expose daemon
herdr-expose panic
herdr-expose status                # confirm; then `expose start` to re-arm
```

For a `lan` or `quick` share, `cloudflare_not_applicable: true` accompanies
`dns_gone` / `tunnel_gone`. Those mean **"there was never one"**, not "we
deleted it".

---

## The daemon will not start, or starts twice

**`port: FAIL — 21118 is held by something that is not herdr-expose`.** Find it
and decide; do not loop on retry. An orphan holding the port produces
`EADDRINUSE` → retry → duplicate connectors → an endless reconnect loop, which
is where a whole evening goes.

**Starting a second copy by hand while a service manager supervises the first.**
Killing the process just makes the manager respawn it, and a manual start
produces two processes fighting for the port. `doctor` tells you which world you
are in:

```
service manager   PASS   launchd supervises herdr-expose
service manager   SKIP   none — the daemon is self-supervised
```

Route start/stop/restart through the same layer that is in charge
(`herdr-expose service status|install|uninstall`, or `stop`/`daemon` when it is
self-supervised).

**`herdr binary: FAIL — not found`, but it works in your shell.** launchd and
`systemd --user` start with a minimal `$PATH` that contains none of
`~/.local/bin`, `~/.cargo/bin`, `~/bin`, `/opt/homebrew/bin`. A bare `herdr`
fails to exec there. The tool resolves an absolute path before any spawn
(`$HERDR_BIN_PATH` → `PATH` → those directories) and fails with a real error
rather than looping; set `$HERDR_BIN_PATH` in the unit if your install is
somewhere unusual.

---

## `--domain` fails before creating anything

That is by design — a half-provisioned tunnel with no DNS record, or a DNS
record pointing at no tunnel, is worse than a clean failure. The preconditions
are checked first.

```
cloudflare token   FAIL   $CLOUDFLARE_ALLPURPOSE_TOKEN is not set
                   hint:  export a token with Account:Cloudflare Tunnel:Edit,
                          Zone:Zone:Read and Zone:DNS:Edit
cloudflare token   FAIL   cannot reach the zone for x.example.com
                   hint:  the token must have Zone:Zone:Read and Zone:DNS:Edit
                          on that zone, and the zone must be in an account the
                          token can see
cloudflared        FAIL   not on PATH
                   hint:  install it (`brew install cloudflared`) — without it
                          the tunnel cannot start and herdr-expose falls back
                          to LAN mode
```

**The zone must be in an account your token can reach.** An arbitrary domain you
own elsewhere does not work.

**If you only need a URL, you do not need any of this.** The default LAN rung and
`--local` need nothing at all, and `--quick` needs nothing but `cloudflared` —
no account, no zone, no token, no DNS.

---

## The exposure is not the rung I asked for

**Read `.share.mode` from the JSON, and `fell_back`.** Nothing ever escalates:
a bare `share` is LAN and can never become a tunnel because `[expose] domain`
happens to be set (ADR 0029). Fallback only goes **down**, and it prints the
reason — an explicit `--quick` or `--domain` with no `cloudflared` resolvable
degrades to LAN, which is the nearest honest answer to "make me reachable".

Two things that confuse people here:

- A `--domain` share reports `mode: "cloudflare"`, not `"domain"`. `cloudflare`
  is the resolved *transport*; `domain` is the *rung* you asked for.
- The top-level `herdr-expose --help` still describes a withdrawn auto-escalating
  ladder (`none = auto: domain if usable, else quick...`). **The binary does not
  do that.** `herdr-expose share --help` is the correct text.

---

## An agent pane in the browser looks like garbage, or the local pane moved

**If the remote view shows a fragment of a screen in a void:** you are in
terminal (`term`) view on an agent pane. Switch to transcript (`text`) view,
which is the default for every pane and is a reflowed, ANSI-stripped rendering
of what the agent is displaying. Mirroring an agent's TUI grid on a phone cannot
be made to work (ADR 0030) — the server only has the post-layout character grid,
so reflowing is impossible.

**If opening a pane in the browser moved the pane you were typing in:** you took
the `term` opt-in and then sent a `resize`. `resize` is the one action in this
product that changes a pane **for everyone attached to it**, including your own
local terminal. Merely viewing never does — a live attach passes no size and
renders at whatever Herdr reports (ADR 0031).

Undo it:

- In the UI, use the "match pane" control.
- On the wire, `{"type":"resize","data":{"target":"<t>","match":true}}` hands
  the geometry back.

**If an agent's `working` badge never clears:** the tree is re-read every 1.5s
while at least one client is connected, and not at all otherwise. A stale badge
with no client attached is expected; a stale badge with a client attached is a
bug worth reporting.

---

## Nothing in the logs

```bash
herdr-expose logs --path         # where it would read
herdr-expose logs -n 200 --json  # structured
herdr-expose logs --share <id>   # a share's own log, which dies with the share
```

The default is a **file** in the state directory, not stderr, because a detached
daemon's stderr goes nowhere and "no way to see what the daemon did" is the
problem being solved (ADR 0032). It rotates at `[log] max_size_mb` keeping
`[log] keep` files. `doctor`'s `log` check reports the path, size and
writability.

**Pane bytes and secret values are never logged, at any level including debug.**
Terminal output is the user's screen and can contain anything; tokens appear as
ids or hashes only. If you are looking for a token value in a log to debug
pairing, stop — it is not there, and that is deliberate.

---

## Reporting a bug

`herdr-expose config show` is safe to paste: the config file contains no
secrets by construction (ADR 0033). Include `doctor` output, the relevant
`logs` lines, and the `mode` from `status`. Redact your domain if you would
rather not publish it — but note that the daemon does not log token values, so
there is nothing else to scrub.
