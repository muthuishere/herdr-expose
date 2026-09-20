---
name: herdr-share
description: Turn a Herdr terminal session — a running coding agent, a shell, one pane — into a private web URL somebody can open on a phone, scoped to that session alone, time-boxed, pairing-gated, and gone when it expires. Use when the owner says "share this", "share this session/pane/agent/terminal", "expose this agent", "give someone access to this", "let X see what this agent is doing", "put this on a URL", "I want to watch this from my phone", "give me a link for this", "put this agent on <hostname>", "quick share this", "share it without a domain", or "share it on the wifi" — and for the whole lifecycle afterwards, "list my shares", "how long has that share got", "extend that share", "revoke the share", "stop sharing", "stop all shares", "kill every exposure", "panic", or "is anything of mine exposed right now". Do NOT use it for exposing a port or a web service in general (it shares Herdr panes only), for publishing or deploying code, for npm or PyPI publishing, for exposure in the photographic, financial or risk sense, or for granting access to a repo, a document or a cloud account. Wraps the herdr-expose CLI — one scoped detached server on a LAN address, a throwaway *.trycloudflare.com tunnel, or a Cloudflare named tunnel on a domain you own.
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
herdr-expose status                          # human-readable: mode, listen, url, devices, sessions
herdr-expose expose status | jq -r '.mode, .url, .running, .healthy'
herdr-expose doctor                          # pass/fail per dependency, with a fix hint on each failure
```

**`status` has no `--json`** — its only flag is `--watch`. For a machine-readable
answer use `herdr-expose expose status`, which always prints JSON
(`mode`, `bind`, `port`, `lan_ip`, `url`, `secure_context`, `fell_back`,
`running`, `healthy`, `provider`, `tunnel`, `pid`, `restarts`, `last_error`), or
`herdr-expose doctor --json`. Piping `status --json` into `jq` silently feeds it
human text and fails.

- The **default (LAN) rung and `--local` need nothing at all**, and `--quick`
  needs nothing but the tunnel helper binary (`cloudflared`). No account, no
  zone, no token, no DNS. There is no `--provider` flag — Cloudflare is the one
  built-in transport, and anything else is a JS adapter selected with
  `adapter = "<id>"` under `[expose]`. If all you need is a URL, that is the whole precondition — and
  if the helper is missing, the CLI says which binary it wanted, so quote that
  rather than guessing.
- `--domain` is the only rung with real preconditions:
  - The **domain's zone must live in the Cloudflare account** the token reaches.
    `*.deemwar.com` works. An arbitrary domain does not. The CLI checks zone
    access before creating anything and fails clearly — do not pre-empt it with
    your own guess.
  - `$CLOUDFLARE_ALLPURPOSE_TOKEN` and `$CLOUDFLARE_ACCOUNT_ID` must be in the
    environment. **Reference them by name only.** Never echo, print, log or
    interpolate their values, and never pass them on a command line.

## Create a share

### Pick the rung the owner actually asked for

Four rungs. **The ladder is climbed, never guessed** (AMENDMENTS 16): no flag
means LAN, and every rung above that is an explicit request.

| rung | flag | reach | needs | cleans up |
|---|---|---|---|---|
| **lan** | **none** (default) | this machine + this network | nothing | process + state |
| quick | `--quick` | anyone on the internet | cloudflared | process + state |
| domain | `--domain X` | the internet, on a name the owner owns | cloudflared + a zone the token reaches | process + state + DNS + tunnel |
| local | `--local` | this machine only (testing) | nothing | process + state |

**When the owner just says "share this" / "give me a URL for this pane", run a
bare `herdr-expose share`.** That is LAN, and it is almost always what they
meant: a phone on the same wifi opens it. Do NOT reach for `--quick` to be
helpful — that is the public internet, and putting an agent session there is
the owner's call to make, not yours. Ask, or wait to be told.

Escalate only on an actual signal:

| the owner says | run |
|---|---|
| "share this", "give me a URL", "let me open it on my phone" | `herdr-expose share --json` |
| "I'm not on the same wifi", "send it to <someone elsewhere>", "public link", "quick share" | `herdr-expose share --quick --json` |
| "put it on <hostname>", "use our domain", "a stable link" | `herdr-expose share --domain X --json` |
| "just for me to test locally" | `herdr-expose share --local --json` |

```bash
# THE DEFAULT. This network, one hour. No domain, no DNS, no tunnel; works offline.
herdr-expose share --json

# Public, in about ten seconds, with NO Cloudflare account and nothing to
# clean up afterwards. Only when the owner asked to be reachable off the wifi.
herdr-expose share --quick --json

# Public, on a hostname in a zone the Cloudflare token reaches.
herdr-expose share --domain agent1.deemwar.com --json

# A specific session, or a single pane, for longer.
herdr-expose share --domain review.deemwar.com --session crypto-desk --days 7 --json
herdr-expose share --quick --pane herdr-plugins/w2:p1 --hours 4 --json

# --pane is REPEATABLE: several panes, one share, one scope.
herdr-expose share --pane herdr-plugins/w2:p1 --pane herdr-plugins/w2:p3 --json
```

Full flag set for a create — there are no others, and an unknown flag is a hard
error: `--local` · `--lan` · `--quick` · `--domain X` · `--session NAME` (`-s`)
· `--pane TARGET` (`-p`, also spelled `--only-target`, **repeatable**) ·
`--hours N` · `--days N` · `--name NAME` · `--json`.

Defaults: the session you are running in (`$HERDR_SESSION`, else resolved from
`$HERDR_SOCKET_PATH`), `--hours 1`, and the **LAN** rung. `--hours` and `--days`
**add together**; the TTL must be positive and under a year. `--local`, `--lan`,
`--quick` and `--domain` are mutually exclusive, and passing two is an error
rather than a precedence puzzle.

`--name` is a label. It only becomes part of a hostname in one narrow case:
`[share] default_mode = "domain"` **plus** a configured `[share] domain_suffix`,
where `--name review` resolves to `review.<suffix>`. A suffix on its own never
promotes a share to the internet. `[share] default_mode`
(`local | lan | quick | domain`, shipped `lan`) is the **only** config key that
can move a bare `share` off the LAN, and an unrecognised value reads as `lan`.

`[share] max_concurrent` (default 10) refuses a create once that many shares are
live, naming the fix. `[share] default_hours` overrides the one-hour default.

**Nothing escalates on its own.** A bare `share` never becomes a tunnel because
`[expose] domain` happens to be configured — that auto-escalation was withdrawn
precisely because it published sessions nobody asked to publish. Report the rung
you **got**, not the one you asked for:

```bash
herdr-expose share --json | jq -r '.share.mode, .share.url, .share.fell_back, .share.secure_context'
```

**`.share.mode` is the resolved TRANSPORT, not the rung name.** It is one of
`local`, `lan`, `quick`, `cloudflare` — so a `--domain` share reports
`"cloudflare"`, never `"domain"`. A non-empty `.share.fell_back` means an
explicit tunnel request degraded; its absence means you got what was asked for.
The create output is
`{"share": {...}, "pairing_code": "...", "pairing_expires_at": "..."}`.

**Fallback only goes down, loudly.** An explicit `--quick` or `--domain` with no
cloudflared installed degrades to LAN and prints why. If the owner needed the
public URL, install cloudflared and re-run — do not paper over it.

**`--domain` never takes the permanent deployment's hostname** — that would
hijack the daemon's own URL, and a share may only ever create or delete records
tagged `herdr-expose-share`. That collision is a hard error naming the fix, not
a silent downgrade.

**The one thing to say when you hand over a quick URL:** its hostname is new
every time, and device tokens are origin-bound, so a phone paired to a previous
quick share has to pair again. Fine for a throwaway, wrong as a daily driver —
which is why `[expose]` keeps a static domain. The CLI prints this on create.

Returns the URL, a **pairing code**, a QR, and the exact expiry. Give the person
the URL and the code through whatever channel you are already talking to them
on. The code is single-use with a short TTL.

**Never paste a pairing code into a public channel or a commit.** It is a
credential. Hand it over the same way you would a password, or let them scan the
QR off the screen.

## Manage

```bash
herdr-expose share list --json          # id, scope, domain, url, expires_at, remaining, alive
herdr-expose share extend <id> --hours 2 | --days N   # [--json]
herdr-expose share pair <id> [--name NAME] --json     # another code for a LIVE share
herdr-expose share revoke <id> --json   # immediate, idempotent (aliases: stop, destroy)
herdr-expose share restore --json       # reap expired, respawn crashed (daemon does this on boot)
```

`share list` also accepts `ls`, and it **reaps** any share whose deadline passed
as a side effect of listing — so a `list` that returns `[]` is a stronger
statement than a cached view. Per-share fields: `id`, `mode`, `scope`,
`session`, `domain`, `url`, `secure_context`, `fell_back`, `state`,
`created_at`, `expires_at`, `remaining`, `remaining_seconds`, `pid`, `alive`,
`expired`, `error`.

`share pair` is how you add a second person without restarting the share. Each
code is single-use, so mint one per device rather than resending the same one.

**Read a share's own log** when one misbehaves — it is a separate file that dies
with the share:

```bash
herdr-expose logs --share <id> -n 100 [--json] [--follow]
herdr-expose logs --path                       # where the daemon's own log lives
```

## Stop everything

```bash
herdr-expose share revoke --all         # every share destroyed; main deployment untouched
herdr-expose panic                      # the above PLUS the main tunnel down; loopback only
```

Use `panic` when the owner says stop/kill/shut it down and means all of it.
**Then re-read the real state and report what is actually gone**, do not report
what you attempted:

```bash
herdr-expose share list --json                       # expect []
herdr-expose expose status | jq -r '.mode, .url, .running'
herdr-expose status                                  # human-readable; NO --json flag exists
```

`panic --json` returns
`{"shares_revoked": [...], "main_tunnel": {"stopped": bool, "halt_file": "...",
"note": "..."}, "ok": bool}`. It stops the main tunnel but deliberately does
**not** destroy it — the permanent DNS record and named tunnel stay, because
that endpoint is meant to be static. **Re-arm with `herdr-expose expose start`**,
which also clears the halt flag; the daemon picks it up within about five
seconds.

Teardown reports per component — `process_gone`, `dns_gone`, `tunnel_gone`,
`port_free`, `state_wiped` — and both commands exit non-zero if anything
survived. For a `lan` or `quick` share `cloudflare_not_applicable:true` comes
with them: nothing was ever created in Cloudflare, so `dns_gone` / `tunnel_gone`
mean "there was never one", not "we deleted it". `revoke --all` and `panic`
handle a mix of lan + quick + domain shares in one pass, and one transport's
failure never aborts the others. A kill switch that silently
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
- **A share is pairing-gated**, never open, at **every** rung — because it
  drives a real agent and can run arbitrary commands. No HTTP endpoint mints or
  reveals a pairing code; it is displayed only on the physically present
  machine. That is the whole reason a LAN address is defensible: somebody on the
  same wifi can reach the port and get precisely nowhere without looking at your
  screen. (The one exception is `--local`, where loopback plus Origin/Host
  pinning replaces the token — and loopback already implies shell on the box.)
- **A LAN share is not "trusted because it is local".** Coffee-shop wifi is a
  LAN. Pairing is required there exactly as it is on a public URL, and scope is
  enforced identically. There is no LAN bypass and you should not ask for one.
- **A quick share is not "safer because the hostname is random".** It is on the
  public internet and no more trusted than a domain share: same pairing, same
  server-side scope, same three-way expiry. Do not treat an unguessable URL as
  a credential.
- **No PWA install on a LAN share.** Plain HTTP on an IP is not a secure
  context (`localhost` is exempt, `192.168.x.x` is not), so no service worker
  and no Add to Home Screen — `secure_context:false` in the JSON. Say so when
  handing over the URL. `--quick` and `--domain` are both https and both report
  `secure_context:true`; only `--domain` gives a hostname stable enough for the
  install to still be paired tomorrow.
- **A quick share's hostname is not stable, on purpose.** It changes on every
  new share (and on a restart within one — `share list` re-reads the live one).
  Paired device tokens are origin-bound and do not follow it. Re-pair; never
  hand out a hostname you read five minutes ago.
- **A LAN share's IP can move under DHCP**, which silently invalidates a printed
  QR. `share list` re-resolves, and `share pair <id>` re-issues at the current
  address. Prefer re-pairing over resending an old link.
- **Expiry destroys DNS — for a `--domain` share.** Unlike the permanent
  deployment, it deletes its record and tunnel when it ends: a hostname
  resolving to a tunnel that no longer exists is worse than no record. A
  `--quick` share has neither, so expiry is simply "the process stops and the
  tunnel dies with it" — nothing to revoke in anybody's account.
- **`--quick` is for shares only.** The permanent deployment (`[expose]
  cloudflare = true`) still REQUIRES `domain` and can never become ephemeral;
  `quick` is not a key any config file can set. Do not try to make the daily
  driver quick — that breaks PWA install, bookmarks and every paired device.
- **Confirm before sharing a session that is not the current one**, and always
  before sharing anything touching money or production — `crypto-desk` runs a
  live trading desk. Read back the scope and the TTL and get a yes.
- **Confirm scope AND TTL out loud before creating anything above `--local`.**
  Read back which session or panes, which rung, and until when — then get a yes.
  A share is not a link, it is a live control channel into a running agent.
- Prefer the shortest TTL that does the job. Extending is one command; a share
  that outlived its purpose is an open door nobody is watching.
- **Never paste a pairing code, a share URL or a device token into a public
  channel, an issue, a commit or a log.** The code is a credential with a
  ten-minute life; hand it over the way you would a password, or let the person
  scan the QR off the screen.
- **Never read or echo `$CLOUDFLARE_ALLPURPOSE_TOKEN` / `$CLOUDFLARE_ACCOUNT_ID`.**
  Reference them by name; the CLI reads them at point of use and never logs,
  stores or returns them.

## Answering "is anything of mine exposed right now?"

```bash
herdr-expose share list --json | jq -r '.[] | "\(.id)  \(.mode)  \(.scope)  \(.url)  \(.remaining)"'
herdr-expose expose status | jq -r '.mode, .url, .running, .healthy'
herdr-expose doctor
```

`doctor`'s `shares` and `exposure` checks answer this in one screen, and its
other checks (`cloudflared`, `cloudflare token`, `dns`, `port`,
`service manager`, `log`, `herdr socket`) are where to look when a share will
not start. Every failing check carries a hint naming the fix — quote the hint
rather than guessing.

## Verbs this skill does NOT cover

`serve`, `daemon`, `stop`, `open`, `pair`, `devices`, `install-service`,
`uninstall-service`, `service`, `config`, `version` — those are the **permanent
deployment**, not sharing. Touch them only when the owner is clearly talking
about the daemon itself. In particular `herdr-expose stop` stops the main
server for everybody; it is not how you end a share (`share revoke <id>` is).

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

## Known stale text in the CLI itself

The **top-level** `herdr-expose --help` still describes the withdrawn
auto-escalating ladder (`none = auto: domain if usable, else quick if
cloudflared is installed, else lan`) and omits `--local`. **The binary does not
behave that way.** `herdr-expose share --help` is correct and matches this
skill. If you are reasoning from `--help` output, use the subcommand's.

## Where the details live

- `docs/troubleshooting.md` — the failures that look like bugs and are not:
  a stale NXDOMAIN cache making a live tunnel look dead, a quick hostname
  changing under a paired device, no PWA install on a LAN IP, `panic` reporting
  `cloudflared STILL RUNNING`.
- `docs/api.md` — the wire contract, if you are building a client rather than
  driving the CLI.
- `docs/adr/0026`–`0029` — why a share is a separate scoped process, why quick
  tunnels exist only for shares, and why the ladder is never climbed for you.
