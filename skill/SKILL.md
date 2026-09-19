---
name: herdr-share
description: Expose ONE Herdr agent session as a private web URL on a domain you own, for a bounded time, then have it destroy itself. Use when the owner says "expose this session", "share this agent", "give me a URL for this pane", "put this agent on <domain>", "let someone see this agent", "share my terminal", "list my shares", "extend that share", "revoke the share", "stop all shares", or "panic / kill every exposure". Wraps the herdr-expose CLI: one scoped detached server + a Cloudflare named tunnel, always time-boxed, always pairing-gated.
---

# herdr-share

Turn the session you are working in into a URL someone can open on a phone,
scoped to that session alone, alive for a fixed window, gone afterwards.

**The binary is the product; this skill is a thin wrapper.** Every verb below is
`herdr-expose ...`. Never reimplement the logic here, never hand-roll
`cloudflared`, never touch DNS directly — the CLI does all of it idempotently
and knows how to clean up after itself.

## Preconditions (check once, fail fast)

```bash
command -v herdr-expose || echo "build it: cd ~/muthu/gitworkspace/herdr-plugins/herdr-expose && ./scripts/build.sh"
herdr-expose status --json | jq -r '.mode, .listen'
```

- The **domain's zone must live in the Cloudflare account** the token reaches.
  `*.deemwar.com` works. An arbitrary domain does not. The CLI checks zone
  access before creating anything and fails clearly — do not pre-empt it with
  your own guess.
- `$CLOUDFLARE_ALLPURPOSE_TOKEN` and `$CLOUDFLARE_ACCOUNT_ID` must be in the
  environment. **Reference them by name only.** Never echo, print, log or
  interpolate their values, and never pass them on a command line.

## Create a share

```bash
# This session, one hour (the default). Most common case.
herdr-expose share --domain agent1.deemwar.com --json

# A specific session, or a single pane, for longer.
herdr-expose share --domain review.deemwar.com --session crypto-desk --days 7 --json
herdr-expose share --domain pair.deemwar.com --pane herdr-plugins/w2:p1 --hours 4 --json
```

Defaults: the session you are running in, and `--hours 1`.

Returns the URL, a **pairing code**, a QR, and the exact expiry. Give the person
the URL and the code through whatever channel you are already talking to them
on. The code is single-use with a short TTL.

**Never paste a pairing code into a public channel or a commit.** It is a
credential. Hand it over the same way you would a password, or let them scan the
QR off the screen.

## Manage

```bash
herdr-expose share list --json          # id, scope, domain, url, expires_at, remaining, alive
herdr-expose share extend <id> --hours 2 | --days N
herdr-expose share pair <id> --json     # another pairing code for a share already running
herdr-expose share revoke <id>          # immediate, idempotent
herdr-expose share restore --json       # reap expired, respawn crashed (daemon does this on boot)
```

`share pair` is how you add a second person without restarting the share. Each
code is single-use, so mint one per device rather than resending the same one.

## Stop everything

```bash
herdr-expose share revoke --all         # every share destroyed; main deployment untouched
herdr-expose panic                      # the above PLUS the main tunnel down; loopback only
```

Use `panic` when the owner says stop/kill/shut it down and means all of it.
**Then re-read the real state and report what is actually gone**, do not report
what you attempted:

```bash
herdr-expose share list --json          # expect []
herdr-expose status --json | jq -r '.mode, .url'
```

Teardown reports per component — `process_gone`, `dns_gone`, `tunnel_gone`,
`port_free`, `state_wiped` — and both commands exit non-zero if anything
survived. A kill switch that silently
half-works is worse than none — if the exit code is non-zero, say so plainly and
name what is still up.

## What is actually exposed

A share is **scoped in the server, not the UI**. The shared instance's tree
contains only the shared scope; every other session is absent from the tree,
absent from subscribe, and rejected at the hub. The command surface narrows to
prompt / send-keys / read / scroll / resize. So sharing one session cannot leak
another, even to a malicious client.

Each share is its own detached process on its own port with its own auth store.
The owner's permanent deployment keeps running untouched.

## Rules that are not negotiable

- **Every share is time-based. There is no permanent mode.** A long-lived share
  is a long TTL (`--days 30`), not a different thing. Do not look for a flag to
  disable expiry; there isn't one, by design.
- **A share is pairing-gated**, never open, because it is on the public
  internet and drives a real agent. No endpoint mints a pairing code — it is
  shown only on this machine.
- **Expiry destroys DNS.** Unlike the permanent deployment, a share deletes its
  record and tunnel when it ends. That is deliberate: a hostname resolving to a
  tunnel that no longer exists is worse than no record.
- **Confirm before sharing a session that is not the current one**, and always
  before sharing anything touching money or production — `crypto-desk` runs a
  live trading desk. Read back the scope and the TTL and get a yes.
- Prefer the shortest TTL that does the job. Extending is one command; a share
  that outlived its purpose is an open door nobody is watching.

## Two behaviours that look like bugs and are not

**A revoked share can sit in `orphaned` for ~60s.** Cloudflare refuses to delete
a tunnel until its edge connections are marked inactive, measured at about a
minute after cloudflared dies. Teardown retries, and deliberately KEEPS
`share.json` (credentials wiped immediately, state `orphaned`) so the next sweep
finishes the job. Deleting the paperwork would strand the tunnel with nothing
left to clean it up. Report it as "tearing down", not as failure, and re-check
with `share list --json`.

**`panic` needs the daemon running the current binary.** The halt flag it raises
is polled by the daemon — killing cloudflared from another process would
otherwise just make the supervisor respawn it. If `panic` reports
`cloudflared STILL RUNNING` and exits non-zero, the daemon is on an older build:
restart it (`herdr-expose stop && herdr-expose daemon`) and run `panic` again.
That output is honest, not cosmetic — treat non-zero as "still exposed".
