# Troubleshooting

Every entry here is a failure that actually happened while building this, not a
hypothetical. Each one is the same shape: **a confusing symptom, a boring
cause**, and what `herdr-expose doctor` tells you about it.

Start with:

```bash
herdr-expose doctor          # pass/fail per dependency, with a hint on each failure
herdr-expose status          # mode, bind, url, reach, devices, sessions
herdr-expose logs -n 100     # the ONLY way to see what a detached daemon did
```

`doctor` checks, in order: `config`, `state dir`, `herdr binary`,
`herdr sessions`, `herdr socket`, `port`, `service manager`, `exposure`,
`shares`, `log`. Read the check name first; the hint under a failure names the
fix.

`doctor` answers for **this binary**. A tunnel or reverse proxy you pointed at
the port is invisible to it — if the question is "is anything of mine on the
internet", that is `cftunnel list`, or wherever you started it.

---

## My hostname stopped answering after an upgrade

**Symptom.** `https://herdr.example.com` used to load and now does not.
`herdr-expose status` reports `mode=local` and a loopback URL, and the log has a
line naming keys in your config.

**Cause: herdr-expose no longer raises tunnels.** `cloudflare = true`, `domain`,
`tunnel_name` and `adapter` are inert — the Cloudflare client, the DNS
reconciliation and the `cloudflared` supervisor were all removed
([ADR 0038](adr/0038-no-tunnel-the-lan-is-the-top-rung.md)). The daemon still
starts and still serves; it just serves loopback, because `lan` was never set.

This was the one failure mode worth designing for, which is why the keys are
**reported** rather than silently ignored:

```
WARN this config asks for a tunnel, and herdr-expose no longer raises one
     keys="cloudflare = true, domain \"herdr.example.com\""
     serving="loopback and the LAN only"
     to_reach_it_from_outside="point a tunnel at the port yourself, e.g. cftunnel add <port> <name>"
```

**Fix.** Raise the tunnel yourself and leave the daemon where it is:

```bash
cftunnel add 21118 herdr        # hostname + health check, one command
cftunnel list                   # confirm it is live and answering 200
```

Your paired devices keep working, because the origin they were paired against is
the hostname — and that has not changed, only who serves it. If you also want
the wifi to reach the machine directly, set `lan = true` and restart.

The dead keys leave your config file the next time herdr-expose rewrites it. You
can delete them by hand now; nothing reads them either way.

---

## `--quick` and `--domain` are refused

**Symptom.**

```
$ herdr-expose share --quick
--quick was removed: herdr-expose no longer raises tunnels. It serves loopback
and the LAN, and nothing else. To reach this from outside the network, point a
tunnel at the port yourself (cftunnel add <port> <name>) and leave this share
on --lan
```

**This is deliberate, and the alternative was considered.** Accepting the flag
and creating a LAN share instead would "work" — and then the link you sent
somebody would not open, and neither of you would find out until they tried it.
An error you see immediately beats a share that lies.

**Fix.**

```bash
PORT=$(herdr-expose share --json | jq -r '.share.port')
cftunnel add "$PORT" review
# hand over the cftunnel URL and the pairing code the share printed
```

The pairing code is unaffected: it is minted locally and shown only on this
machine, so it never travels over whatever you put in front of the port.

**Three things the tunnel does not inherit from the share.** It does not expire
when the share does, `share revoke` does not take it down, and `panic` cannot
see it. When the share ends, the hostname stays up and stops answering — take
it down yourself (`cftunnel stop review`).

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
`share list --json` carries `secure_context: false`.

**Fix — pick the trade-off deliberately.**

| want | do | cost |
|---|---|---|
| a working web app on the wifi, now | nothing; this is working as designed | no install, no offline shell |
| an installable app | put HTTPS in front of the port — `cftunnel add 21118 herdr`, or any tunnel or reverse proxy | a second thing to run and to take down |

Use a **stable** hostname if you install it. A PWA is pinned to its origin and
device tokens are origin-bound, so a hostname that changes on every restart
breaks the install and the pairing together.

A native mobile client is unaffected by any of this — it talks to the same API
(`docs/api.md`) and does not care about browser secure-context rules.

---

## `panic` or `revoke` exits non-zero and says something is still up

**Treat a non-zero exit as "still exposed."** Both commands report three facts
per share — `process_gone`, `port_free`, `state_wiped` — each re-read from the
world rather than assumed, and deliberately fail loudly rather than claiming
success. A kill switch that silently half-works is worse than none.

**`port STILL BOUND` on a share that died cleanly.** A listening socket is not
closed synchronously with the process that owned it. Teardown waits for the port
rather than asking once (this was reproducible on Windows, where a share is
stopped with `TerminateProcess` rather than a signal it can handle), but a very
loaded machine can still outlast the wait. Re-check before acting:

```bash
herdr-expose share list --json     # expect []
```

**What `panic` does NOT do, by design.** It revokes shares. It does not stop the
daemon — that is `herdr-expose stop` — and it cannot stop a tunnel you pointed
at the port, because that process is not its to kill. The JSON says so in
`daemon.note`, and it previously did something worse: it wrote an `EXPOSE-HALT`
flag that the tunnel supervisor used to poll, and after the supervisor was
removed nothing read that path at all. A kill switch reporting success for
writing an unread file is the exact failure this entry exists to prevent, so the
flag is gone.

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

## A share created by an older build will not restart

**Symptom.** After an upgrade, `herdr-expose share restore` (which the daemon
runs on every start) reports:

```
share 9f2a1c04 was created on the "cloudflare" rung, which herdr-expose no
longer has (it serves loopback and the LAN, and nothing else). Revoke it with
`herdr-expose share revoke 9f2a1c04` and create a new one; if you need it
reachable from outside this network, point a tunnel at the new share's port
(cftunnel add <port> <name>)
```

**Cause: the share's record names a rung that was removed.** Its `share.json`
says `mode: "cloudflare"` or `"quick"`, and this build cannot serve either.

**Why it refuses instead of doing something sensible.** Quietly bringing it back
on the LAN would be the wrong kindness. It was a public share yesterday;
returning it on `192.168.1.24` under the same id gives you a `share list` entry
whose URL anyone who has the old link will find dead, and paired device tokens
that no longer match its origin. That is a different share wearing its name.

**Fix.** Revoke it and make a new one. Revoking still works — a share this
build cannot *run* is still one it can *wipe*, which is the other half of the
contract:

```bash
herdr-expose share revoke 9f2a1c04
herdr-expose share --session <name> --hours 1 --json
```

---

## The exposure is not the rung I asked for

**Read `.share.mode` from the JSON.** It is `local` or `lan`, and nothing can
escalate because there is no rung above the LAN to escalate to.

The one case that surprises people: **a `--lan` share can report a loopback
URL.** If the machine has no non-loopback IPv4 — wifi off, every interface down
— there is no LAN address to name, so the URL is `http://127.0.0.1:<port>`. The
mode is still `lan` and the socket is still bound to `0.0.0.0`; it will start
answering on the network address as soon as there is one. Saying `127.0.0.1` is
the honest answer rather than inventing a hostname nobody can reach.

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
