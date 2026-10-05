# herdr-expose — full guide

*Short version: the [README](../README.md). This is everything else.*

Your [Herdr](https://herdr.dev) panes, in a browser or on your phone.

One Go binary. It connects to the Herdr socket on your machine, serves a
mobile-first web UI it has embedded inside itself, so you can check on your
agents from your phone.

```
Herdr  --unix socket-->  herdr-expose  --127.0.0.1:21118-->  browser
                              |
                              +--LAN 0.0.0.0:21118----->  your phone, same wifi

                   ...and that is as far as it goes. From anywhere else:

   your phone, anywhere  <--https--  a tunnel YOU run  -->  :21118
                                     (cftunnel / ngrok / ssh -R / tailscale)
```

**The API is the product.** The web UI is its first client; a native mobile app
is meant to be its second, built against [`docs/api.md`](api.md) with no
access to this repository. Everything here is a documented, versioned HTTP +
WebSocket contract.

> ⚠️ **This binary executes arbitrary commands on your machine.** Read
> [Security](#security) before you expose it. It is not optional reading.

---

## What it gives you

- **The pane tree**, not a shrunken desktop TUI: workspaces, tabs, panes, agent
  status, live on your phone.
- **One live terminal** with a real emulator, full colour, real keys — plus
  cheap summary tiles for everything else, so twenty panes do not kill the tab.
- **Blocked-agent Q&A**: when an agent is waiting on a human, you get the
  question and a key bar (y / n / enter / esc / arrows / 1 2 3). Generic across
  every agent kind.
- **DONE badges that are yours.** Seen state is per device, so glancing at a pane
  on your laptop does not wipe the badge on your phone.
- **An installable PWA**: home-screen icon, standalone display, safe-area
  handling. Online only, on purpose — a cached terminal is a lie. (Not available
  in `lan` mode: plain HTTP on a LAN IP is not a secure context.)
- **Loopback by default**, your wifi if you ask, your own domain if you mean it —
  and a device token required in both of the latter.
- **An agent skill in the box.** Installing the plugin also installs
  [`herdr-share`](#the-agent-skill), so you can say "share this session" to
  Claude Code and it runs the right command with the right rung.

---

## Requirements

- Herdr **0.9.0** or newer (`herdr --version`)
- macOS or Linux (amd64 or arm64)
- Optional, at build time only: Go 1.22+ and node 20+. **Neither is needed at
  run time**, and neither is needed to install: without them the build step
  downloads the prebuilt binary for your platform from the latest GitHub
  release and checks it against the published SHA-256. With them, nothing is
  downloaded and everything is built from the source you just cloned.

---

## Install

### As a Herdr plugin (recommended)

```bash
herdr plugin install muthuishere/herdr-expose
```

`herdr plugin install` takes GitHub **shorthand only** — `OWNER/REPO`. A full
`https://github.com/...` URL is not accepted and fails with a confusing
"Repository not found".

It shows you a preview of everything it will run, clones the repo into a managed
checkout under `~/.config/herdr/plugins/github/`, runs `./scripts/build.sh`
there, and registers the actions, panes, link handler and startup hook.

**That one command installs all three parts**, and the build step prints every
path it touches:

| | |
|---|---|
| the **binary**, web UI embedded | `<managed checkout>/bin/herdr-expose` |
| a **PATH symlink**, so `herdr-expose <verb>` works | `~/.local/bin/herdr-expose` |
| the **agent skill** ([below](#the-agent-skill)) | `~/.claude/skills/herdr-share`, plus `~/.agents/skills/herdr-share` if that directory already exists |

Both links point *into* the checkout, so a `git pull` or a rebuild updates them.
Nothing real is ever overwritten: a file that is not a symlink, or a skill
directory you wrote yourself, is reported and left alone. If you would rather
the build touched nothing in your home directory, install with
`HERDR_EXPOSE_NO_LINK=1` — you then get `bin/herdr-expose` and nothing else.

The startup hook runs when Herdr next restores your session, not at install time —
so right after installing, start it yourself:

```bash
herdr plugin action invoke hex:open     # starts nothing; opens the local UI
herdr plugin action invoke hex:status   # what is running, and where
herdr plugin action invoke hex:pair     # show a pairing QR for a phone
```

Every id is namespaced `hex:` because `herdr plugin action invoke` takes
`--plugin` as optional, so a bare `open` would collide with any other plugin
defining one. (`:` is a legal id character in Herdr 0.9.0; `.` is not.) Pass
`--plugin dev.deemwar.herdr-expose` if you want to be explicit.

The binary lives inside the managed checkout, which Herdr owns and replaces on
every reinstall, so the install links it onto your PATH for you. If you skipped
that (`HERDR_EXPOSE_NO_LINK=1`), or `~/.local/bin` is not on your `PATH`, do it
yourself:

```bash
ln -sf ~/.config/herdr/plugins/github/dev.deemwar.herdr-expose-*/bin/herdr-expose \
  ~/.local/bin/herdr-expose
```

Re-link the skill at any time — after moving the checkout, or on a machine whose
`~/.claude` did not exist at build time:

```bash
herdr-expose skill install                        # or:
herdr plugin action invoke hex:install-skill
```

`herdr plugin uninstall dev.deemwar.herdr-expose` removes the checkout. It does
**not** remove `~/.config/herdr-expose/` or `~/.local/state/herdr-expose/` —
your config and device tokens are yours, and deleting them is your
call.

### From source

```bash
git clone https://github.com/muthuishere/herdr-expose
cd herdr-expose
./scripts/build.sh          # web app, Go binary, PATH symlink, agent skill
./bin/herdr-expose serve    # http://127.0.0.1:21118
```

`build.sh` is the install script: after the binary it links
`~/.local/bin/herdr-expose` and installs the `herdr-share` skill, printing each
path. `./scripts/build.sh --no-link` builds and links nothing.

Link it into Herdr without installing from GitHub:

```bash
herdr plugin link .         # link does NOT run the build; build first
herdr plugin unlink dev.deemwar.herdr-expose
```

### From a release binary

No Go, no node, no clone:

```bash
# pick your platform from https://github.com/muthuishere/herdr-expose/releases
curl -LO .../herdr-expose_vX.Y.Z_darwin_arm64.tar.gz
curl -LO .../herdr-expose_vX.Y.Z_SHA256SUMS
shasum -a 256 -c herdr-expose_vX.Y.Z_SHA256SUMS   # do not skip this
tar -xzf herdr-expose_vX.Y.Z_darwin_arm64.tar.gz
./herdr-expose daemon
```

The binaries are **not signed or notarized**. On macOS, Gatekeeper quarantines
anything downloaded with a browser; `xattr -d com.apple.quarantine
./herdr-expose` clears it. If you would rather not trust a binary at all, build
from source — it is one command and it is the default path.

---

## Quickstart

```bash
# 1. start it (or let the plugin startup hook do this for you)
herdr-expose daemon

# 2. open it locally
herdr-expose open

# 3. pair your phone: prints a 6-character code and a QR, valid 10 minutes
herdr-expose pair

# 4. see what is going on
herdr-expose status
```

### Commands

| Command | What it does |
|---|---|
| `serve` | run the server in the foreground (what a service unit runs) |
| `daemon` | fork-exec `serve` detached and return immediately |
| `status` | server state, the URL, who can reach it, paired devices |
| `stop` | stop the running server |
| `pair` | mint a single-use pairing code and show a QR |
| `share [--lan\|--local]` | expose ONE session on a time-boxed, self-destructing URL (no flag = LAN) |
| `share list\|extend\|pair\|revoke\|restore` | manage shares (every verb takes `--json`) |
| `panic` | revoke every share (the daemon itself is `stop`) |
| `service install\|uninstall\|status` | install, remove or report the launchd / systemd unit for crash recovery (also spelled `install-service` / `uninstall-service`) |
| `skill install\|uninstall\|status` | link, unlink or report the `herdr-share` [agent skill](#the-agent-skill) |

---

## Exposing it

**Two modes, and the local network is as far as it goes.** The mode decides the
bind address — **`bind` is not a setting you control**, which is deliberate,
because this binary runs commands.

| mode | reachable from | device token | installable PWA |
|---|---|---|---|
| `local` (the **daemon's** default) | this machine only | not required | yes |
| `lan` (a **share's** default) | anyone on your wifi | **required** | **no** (see below) |

The daemon defaults to `local` and a `share` defaults to `lan`, deliberately:
the daemon serves whoever is sitting at this machine, while a share exists to
be opened from somewhere else. Same principle — the least exposure that still
does the job — two different jobs.

> **herdr-expose does not raise tunnels.** It used to: `cloudflare = true` plus
> a domain made it talk to the Cloudflare API, mint tunnel credentials, upsert a
> DNS record and supervise `cloudflared`. That is all gone — about 6,300 lines
> of it — and [ADR 0038](adr/0038-no-tunnel-the-lan-is-the-top-rung.md) is the
> argument for why. Short version: this binary reads and writes your agent
> sessions, and a tool that forwards a TCP port cannot accidentally forward a
> pane. See [Reaching it from the internet](#reaching-it-from-the-internet).

### local — the daemon's default

Bound to `127.0.0.1`. No token needed: anyone who can reach loopback already has
shell on the box. Origin and Host pinning are enforced instead, which is what
stops a website you are visiting from driving your terminal via DNS rebinding.

```bash
herdr-expose open
```

### lan — your wifi, no setup at all

For the common case of a phone and a laptop on the same network, with no domain
and no account.

```toml
# ~/.config/herdr-expose/config.toml
[expose]
lan = true
```

```bash
herdr-expose stop && herdr-expose daemon
herdr-expose status
# http://192.168.1.24:21118   (also shown as a QR by `herdr-expose pair`)
```

This binds `0.0.0.0`, so **anyone on the network can reach the port** — the
server logs a warning saying so on every start. It is safe because a device token
is mandatory and **the pairing code is only ever shown on this machine's screen**;
no endpoint will hand one out.

> **No PWA install in lan mode.** Plain HTTP on a LAN IP is not a secure context
> (`localhost` is exempt; `192.168.x.x` is not), so the browser will not register
> a service worker or offer to install the app. `status` reports
> `secure_context: false`. You get a working web app in a browser tab, not a
> home-screen app. That is a rule of the web platform, not something we can fix —
> only HTTPS fixes it, and that means putting a tunnel or proxy in front of the
> port yourself.

### Reaching it from the internet

Point something at the port. herdr-expose has no opinion about what:

```bash
# On the machine this was built on, one command — hostname and health check:
cftunnel add 21118 herdr

# Or anything else that forwards HTTP:
ngrok http 21118
ssh -R 8080:localhost:21118 your-vps
# tailscale funnel, a reverse proxy, a WireGuard peer — all fine
```

Then pair as usual. The pairing code is still minted locally and shown only on
this machine, so **whatever is in front of the port, the credential never
travels over it**. That is what made `lan = true` acceptable and it is what
makes this acceptable.

Three things to be clear about, because they are the ones that bite:

- **The tunnel is not part of herdr-expose.** `herdr-expose panic` cannot stop
  it and does not claim to; `share revoke` does not take it down. Stop it where
  you started it (`cftunnel stop herdr`).
- **`status` and `share list` report the LAN or loopback URL**, because that is
  what this binary knows. The public hostname lives in the tunnel tool — read it
  from there (`cftunnel list`).
- **A tunnel in front of a share outlives the share.** When the share expires
  the hostname stays up and stops answering. Take the tunnel down too.

`--quick`, `--domain` and `--provider` are **refused**, not quietly turned into
a LAN share:

```
$ herdr-expose share --quick
--quick was removed: herdr-expose no longer raises tunnels. It serves loopback
and the LAN, and nothing else. To reach this from outside the network, point a
tunnel at the port yourself (cftunnel add <port> <name>) and leave this share
on --lan
```

A config file still carrying `cloudflare`, `domain`, `tunnel_name` or `adapter`
still **loads** — an upgrade that will not start is the one failure you cannot
work around — and the daemon names those keys once at startup, says it is
serving loopback and the LAN only, and drops them the next time it rewrites the
file. Same for `[share] default_mode = "quick"`, which reads as `lan`.

> **There are no tunnel adapters any more.** The embedded JavaScript host existed
> to let you write a transport Cloudflare was not; with no built-in transport to
> be an alternative to, it went too. **Chat adapters are a different thing** —
> `[[chat.adapters]]`, a different contract, still here, see
> [Driving it from a chat app](#driving-it-from-a-chat-app).

---

## Sharing one session

`expose` puts the whole machine's Herdr tree behind one long-lived URL. `share`
does the opposite: **one session, one URL, one deadline, then gone.**

```bash
herdr-expose share                          # http://<lan-ip>:<port>   THE DEFAULT
herdr-expose share --lan                    # the same, said out loud
herdr-expose share --local                  # http://127.0.0.1:<port>  testing only
```

To let somebody outside your network open it, point a tunnel at the share's own
port and leave the share on `--lan` — see
[Reaching it from the internet](#reaching-it-from-the-internet). The port is in
the JSON: `herdr-expose share --json | jq -r '.share.port'`.

### The ladder is climbed, never guessed (AMENDMENTS 16)

| rung | flag | reach | needs | teardown at expiry |
|---|---|---|---|---|
| **lan** | **none** (default) | this machine **and this network** | nothing | stop the process, free the port, wipe the state |
| local | `--local` | this machine only — an opt-**in** for testing | nothing | the same |

The flags are mutually exclusive, and **no flag means LAN**. There is nothing
above LAN to escalate to, and `--quick` / `--domain` / `--provider` are refused
rather than silently downgraded.

LAN is the default rather than loopback because a share exists to be **opened
from somewhere else** — at this machine you would just use the daemon on
`localhost:21118`. Binding `0.0.0.0` covers loopback and the wifi in one, and
goes no further. (The daemon's own `[expose]` default stays `local`. Same
principle — the least exposure that still does the job — different job.)

Why the ceremony survives a two-rung ladder: this binary runs arbitrary
commands inside your agent sessions, and the difference between "this machine"
and "this café's wifi" must never be a config key you forgot you set. The
resolver still refuses to resolve above what was requested, and that check is
what the whole amputation was held to.

The chosen rung and its reach are printed at create time (`exposure: lan —
anyone on this network`), so you never have to infer it from the URL.

Everything else is identical across both: **scope is enforced in the
server** (the shared instance's tree contains only that session — other sessions
are absent from the tree, from subscribe, and rejected at the hub), **pairing is
mandatory**, and every share is time-boxed three ways (an in-process timer, a
token deadline, and a `share.json` the daemon sweeps). There is no permanent
share; a long one is `--days 30`.

```bash
herdr-expose share list [--json]            # also reaps anything past its deadline
herdr-expose share extend <id> --hours 2
herdr-expose share pair <id>                # another code for a share already running
herdr-expose share revoke <id> | --all      # immediate, idempotent, verified
herdr-expose panic                          # every share down, right now
herdr-expose stop                           # ...and this stops the daemon itself
```

`revoke --all` and `panic` handle every share in one pass, and one failure never
aborts the others. Teardown reports three facts per share — process gone, port
free, state wiped — each re-read rather than assumed, and exits non-zero if
anything survived.

**`panic` does not stop the daemon, and says so.** It revokes shares; the
daemon's own listener is loopback or the LAN and comes down with
`herdr-expose stop`. If you pointed a tunnel at the port, that is outside this
binary entirely — `cftunnel stop <name>`, or wherever you started it.

---

## The agent skills

Two **Claude Code / agent skills** ship in this repository, and both install
through the same `skill install`:

| Skill | Directory | What it is for |
|---|---|---|
| `herdr-share` | `skill/` | Exposing and sharing sessions — the rungs, pairing, revoking |
| `herdr-message` | `skill-message/` | Listing other agents and messaging them, here or on a peer machine |

### `herdr-share`

It is how the owner of this tool actually drives it: you say what
you want in English, the agent runs the CLI.

It is a thin wrapper, on purpose — **the binary is the product.** The skill
never reimplements a rung and never invents a transport. What it adds is
judgement: which rung the words you used actually asked for,
what to read back before creating anything, and what never to paste into a
channel.

```
you:    "share this session so I can watch it from my phone"
agent:  herdr-expose share --json
        -> http://192.168.1.24:49213, pairing code, expires in 1h

you:    "I'm not on the same wifi"
agent:  herdr-expose share --json            (still a LAN share)
        + cftunnel add <that share's port> review
        -> https://review.…, the same pairing code

you:    "is anything of mine exposed right now?"
agent:  herdr-expose share list --json  +  herdr-expose status
```

The rules it works under are in [`skill/SKILL.md`](../skill/SKILL.md), and two are
worth knowing before you let an agent near this:

- **It never climbs the ladder for you.** "Share this" is a LAN share, and a
  public URL is a second, separate step it has to tell you about rather than
  take on your behalf — herdr-expose cannot raise a tunnel, so the agent cannot
  quietly put a session on the internet even if it wanted to.
- **`share --all` needs your explicit yes, in the conversation.** Before running
  it the agent must tell you how many sessions it covers and name them, state
  the rung and the expiry, and wait. The CLI's own typed-`yes` prompt exists for
  a human at a terminal; an agent runs non-interactively and would have to pass
  `--yes`, so the skill forbids passing `--yes` to *skip* asking you. It is only
  allowed after you have already said yes. A scoped share has no such
  ceremony — its blast radius is one session.

### Installing it

The plugin install does this for you (see [Install](#install)). To do it by
hand, or to re-link after moving the checkout:

```bash
herdr-expose skill install      # symlinks BOTH skills into ~/.claude/skills/
herdr-expose skill status       # where each is linked, and whether that resolves
herdr-expose skill uninstall    # remove the links; the checkout is untouched
```

`status` is worth reading rather than skimming: it reports a link that still
resolves but points at **another checkout** as `STALE`, which is what happens
after you move the repo or work out of a second git worktree. `install`
repoints it.

It links into `~/.claude/skills/`, creating it if needed, and into
`~/.agents/skills/` only when that directory already exists.

**It is a symlink, not a copy**, so a `git pull` or a rebuild updates the skill
the agent reads — a copy would quietly fork from the binary it drives. `install`
is idempotent: an already-correct link says so and does nothing, a link left
over from an old checkout location is repointed, and a *real* directory in the
way is refused with the path to move rather than clobbered. `uninstall` only
ever removes symlinks that resolve to this skill.

After installing, start a new agent session — skills are discovered at startup.

---

## Driving it from a chat app

A share gives you a browser. A **chat bridge** gives you Telegram, Teams,
Discord or Slack — the same agents, answered from wherever you already get
messages. `examples/chat-bridge.mjs` is a complete, dependency-free one.

```bash
node examples/chat-bridge.mjs --channel console              # no credentials; prove it first
node examples/chat-bridge.mjs --channel telegram --digest 0  # HERDR_EXPOSE_TELEGRAM_TOKEN
node examples/chat-bridge.mjs --channel telegram --url http://HOST:PORT --code ABC123
```

What arrives in the channel:

```
herdr bridge up on http://127.0.0.1:21118 (local)
/ls  /follow N  /say ...  /key y  /mute  /help

2 sessions
  [s:herdr-plugins] herdr-plugins (10)
  [s:crypto-desk] crypto-desk (15)
```

Tapping a session lists its panes, tapping a pane follows it, and anything else
you type is sent to that pane as a prompt. Where the channel has buttons they
are buttons; where it does not — the `console` channel is the honest case — they
degrade to the `[s:…]` / `[k:y]` payloads **the text commands already accept**,
so a tap and a typed command take the same path and no command logic is written
twice.

### Do not mirror a terminal into a chat window

This is the design decision, and the obvious choice is the wrong one. A pane
redraws constantly; a 120x40 screen pasted every few seconds is unreadable, and
the channel will rate-limit you into a backlog that arrives after it stopped
being true.

So the bridge is a **notifier with a terminal attached**. An agent going
`blocked` is pushed immediately with its question — that is the event a human is
actually waiting for. Everything else is quiet unless something changed.

The thing that makes this work is **not** in the bridge: it subscribes to
[`transcript_settled`](api.md#81-the-four-transcript-flavours), and the server
holds the screen until the agent has stopped *and* the screen has stopped
moving, then sends it once. A client that tries to do this itself writes a timer
and gets it wrong — the measured failure is eight near-identical messages for
one answer, because an agent reporting `done` is often still painting.

### Built in: the `[chat]` table

`examples/chat-bridge.mjs` is a standalone program you run yourself. The daemon
can also own an adapter and keep it alive, which is what `[chat]` configures.

```toml
[chat]
enabled = false            # ships off

[[chat.adapters]]
id = "telegram"
enabled = false            # ships off too
command = "node telegram.js"
  [chat.adapters.env]
  HERDR_EXPOSE_TELEGRAM_TOKEN = "$HERDR_EXPOSE_TELEGRAM_TOKEN"
  HERDR_CHAT_ID               = "123456789"
```

**Both switches must be on.** A bot token sitting in an environment variable is
not consent to start answering messages with it, so finding one is deliberately
not enough. There is no `chat enable` subcommand either: a command this daemon
will execute, and a bot that will answer strangers, are not things a subcommand
should turn on for you.

**`$NAME` and `${NAME}` expand from the environment**, so the config file holds
a *reference* and never a value. `"$HERDR_EXPOSE_TELEGRAM_TOKEN"` is a name —
it can be committed, read aloud, or pasted into a bug report. A missing
variable expands to empty **and is reported**: expanding to the literal
`$TOKEN` would send that string to the API as a credential and produce a
baffling 401, while expanding to empty in silence gives you an adapter that
does nothing for no visible reason.

#### Any runtime, because it is a command

```toml
command = "node telegram.js"      # or
command = "bun telegram.ts"       # or
command = "go run adapter.go"     # or
command = "python3 discord.py"    # or
command = "./my-adapter"
```

It runs with its working directory set to the adapters directory, and it is
**not run through a shell** — no pipes, no redirection, no command
substitution. A chat adapter has no reason to need a shell, and handing one a
shell turns every config file into a place to hide a command.

#### Where adapters live

`~/.config/herdr-expose/adapters/chat/` — beside `config.toml`, because an
adapter is something you *write*, not something the daemon generates. The
bundled `telegram.js` and `template.js` are written there on daemon start and
**never overwritten**: that copy exists to be edited, and an upgrade that
replaced an edited adapter would destroy the work quietly — it would keep
running, just not the way you left it. Delete one to get a fresh copy.

#### Supervision, which gives up

An adapter talks to somebody else's server over somebody else's network, so it
*will* exit, and restarting it is right. Restarting it forever is not: a bad
token fails identically every time, and a supervisor that keeps trying turns
one mistake into a process every few seconds — filling the log with one line
and burning the rate limit that would have let the fixed adapter connect. So
**five restarts with doubling backoff, then down**, said once. A run that
lasted two minutes forgives the count.

#### Seeing what is wrong

```bash
herdr-expose chat status      # every adapter, its state, and why
herdr-expose chat list        # the bundled adapters and the directory
herdr-expose chat seed        # write the bundled adapters out (never overwrites)
```

```
$ herdr-expose chat status
[chat] disabled — nothing will run
! telegram       misconfigured  node telegram.js
    HERDR_EXPOSE_TELEGRAM_TOKEN is not set
    HERDR_CHAT_ID                123456789
    HERDR_EXPOSE_TELEGRAM_TOKEN  $HERDR_EXPOSE_TELEGRAM_TOKEN  (not set)
```

It prints the *reference* and whether it resolves, never the value — this
output gets screenshotted. **`status` exits non-zero when an adapter needs a
person**, so it works in a script; "deliberately off" exits zero, because that
is a different answer from "enabled but broken".

The same states appear in the web UI under the gear, which is read-only: a page
somebody paired from a phone must not be able to enable a bot or rewrite a
command line the daemon will execute.

### Writing a channel is two functions

```js
export function myChannel() {
  return {
    name: 'myapp',
    async poll() { return [{ text: 'what they typed', thread: 'conversation id' }] },
    async send(text, thread, opts) { /* opts.choices = [{label, data}] */ },
  }
}
```

Register it in `CHANNELS`, run `--channel myapp`. Cleaning, code folding,
waiting for the agent, deduplication, menus, commands and the single-instance
lock are already done — in the plugin or in the example. The full contract is in
[`skill/SKILL.md`](../skill/SKILL.md).

### One bridge per channel

Two bridges on one Telegram token both long-poll `getUpdates`, both receive
every update, and **every command runs twice** — which looks exactly like a code
bug. A lock file keyed on the channel makes it impossible rather than unlikely:

```
$ node examples/chat-bridge.mjs --channel telegram
another telegram bridge is already running (pid 45149).
stop it first, or: kill 45149
```

A stale lock from a crashed run is reclaimed after checking whether the pid is
alive — a lock nobody can clear is worse than no lock.

---

## Configuration

`~/.config/herdr-expose/config.toml`, always — the path does not change depending
on whether Herdr or your shell started the process. Created with defaults on
first run. Reloaded on `SIGHUP`. Unknown keys are preserved, so a config written
by a newer version survives an older binary.

```toml
[server]
port = 21118
# no `bind` key: the exposure mode decides it, and it is enforced in code

[auth]
pairing_ttl_seconds = 600    # 10 minutes
device_ttl_days = 30         # sliding, from last use
max_devices = 32

[ui]
theme = "auto"
default_view = "grid"        # grid | focus

[expose]
lan       = false            # bind 0.0.0.0 so the wifi can reach this machine
autostart = false

[share]
default_hours  = 1           # TTL when neither --hours nor --days is given
default_mode   = "lan"       # local | lan — the rung a bare `share` climbs to
max_concurrent = 10
```

`default_mode` ships as `"lan"` and is the only key that moves a bare `share`
between rungs.

**Withdrawn keys keep loading, and are named out loud.** A config that still
carries `cloudflare = true`, a `domain`, a `tunnel_name`, an `adapter`,
`ngrok = true`, a loopback `server.bind` or `default_mode = "auto"` /
`"quick"` / `"domain"` starts normally. The key is read without complaint,
**reported once at startup** with what is being served instead, ignored, and
dropped the next time the file is written.

Both halves of that matter. A key that no longer does anything is not a reason
to take somebody's daemon down over a file they cannot edit until it is back up
— but a `cloudflare = true` that is silently ignored is how you spend an hour
in a DNS dashboard wondering why your hostname stopped answering. So it loads,
and it says so.

**There are no secrets in this file**, and there is nothing left for one to be:
herdr-expose holds no provider credentials. Device tokens live as SHA-256
hashes in the state directory at mode 0600. See
[ADR 0009](adr/0009-toml-config-outside-repo.md) and
[ADR 0017](adr/0017-hashed-per-device-tokens.md).

---

## Security

**Read this part.**

`herdr-expose` talks to Herdr, and Herdr can run commands. `pane.run`,
`pane.send_text` and `agent.prompt` are, by design, **arbitrary command
execution as you, on your machine**. The moment it is reachable off this box —
your wifi, or anything you put in front of the port — this is a remote code
execution endpoint.

That is not a scary edge case. It is the feature. A terminal you cannot type into
is a screenshot. So authentication here is the primary feature, not a checkbox.

### What the tool does about it

- **You do not choose the bind address; the mode does.** `local` binds
  `127.0.0.1`; `lan` binds `0.0.0.0` and says so loudly in the log on every
  start. There is no free-form `bind` setting to get wrong, and no mode that
  reaches past your network.
- **The pairing code is only ever displayed on this machine** — the terminal, or
  the QR pane inside the Herdr TUI. **No HTTP endpoint mints or reveals one.**
  `POST /v1/pair` only accepts codes. That is what makes `lan` mode defensible:
  someone on your wifi can reach the port and get nowhere without your screen.
- **On loopback, no token is required — but Origin and Host are pinned.** The
  real attacker against a local server is the browser: any site you visit can
  call `127.0.0.1`, and DNS rebinding turns a hostile page into a client. Only
  `127.0.0.1:<port>` and `localhost:<port>` are accepted as Origin and Host, and
  a missing Origin on a WebSocket upgrade is rejected rather than allowed.
- **Two secrets**: a server token (may you talk to this instance at all) and
  per-device tokens issued by pairing.
- **Hashes only.** SHA-256, in 0600 state, never plaintext, never in config,
  never in a log, never in `/v1/config`. Every comparison is constant-time.
  The server token is printed once, at generation, and only to a terminal: a
  supervised unit's stderr is the journal, so a daemon started by systemd or
  launchd mints it silently and tells you to run `herdr-expose token rotate`,
  which prints a fresh one and invalidates the old. Paired devices are
  unaffected by a rotation — they hold their own tokens.
- **Pairing codes** are 6 characters, single-use, valid 10 minutes, and
  rate-limited per source address.
- **Device tokens** are 32 random bytes with a sliding 30-day expiry, capped at
  32 devices, individually revocable and listable (user-agent and last-seen IP —
  never the hash).
- **Origin allowlist checked before any CORS header is echoed**, plus
  `frame-ancestors 'none'`, `X-Frame-Options: DENY`, and handshake rate limiting.
  Auth is verified **before** the WebSocket upgrade.
- **Nothing is cached by the service worker under `/v1/*`** — no offline replay
  of a terminal, ever.

### What you have to do about it

- **Do not leave a tunnel up.** If you pointed one at the port, take it down
  when you are done (`cftunnel stop <name>`). herdr-expose cannot do this for
  you — `panic` does not touch it and does not pretend to — so it is genuinely
  your job to remember.
- **Do not leave `lan = true` on at a conference or a coffee shop.** It is meant
  for your own network.
- **Revoke devices you no longer use.** `herdr-expose status` lists them;
  `herdr-expose devices --revoke <id>` removes one.
- **Whatever you put in front of the port terminates TLS.** Cloudflare, ngrok,
  your own reverse proxy — it can see the traffic. This is not end-to-end
  encrypted and nobody should imply otherwise.
- **Treat the URL as a credential** even though it is not one. A public URL plus
  an unpatched auth bug is a shell; do not paste it into a group chat.
- **Put an identity layer in front of it on a work machine** — Cloudflare
  Access, an mTLS proxy, a VPN. Pairing is good; two gates are better.

Found a security problem? Open a private security advisory on the repository
rather than a public issue.

---

## How it works

Short version, with the details in the ADRs:

- **Transport and UI only.** Herdr does the work; we never reimplement it, never
  parse its TUI, never read its state files ([ADR 0002](adr/0002-socket-api-is-the-only-contract.md)).
- Everything is the **socket API**, except terminal bytes, which Herdr 0.9.0 does
  not expose there — those come from the `herdr terminal session observe|control`
  subprocess ([ADR 0003](adr/0003-terminal-streaming-via-subprocess.md)).
- **Split plane on one WebSocket**: JSON control frames, and binary data frames
  with an 11-byte header. No base64 on the wire, ever
  ([ADR 0006](adr/0006-split-plane-json-control-binary-data.md)).
- **The server picks the mode** — live, summary or none — so a client cannot ask
  for twenty live terminals ([ADR 0007](adr/0007-server-owned-viewport-modes.md)).
- **Predictive local echo**, Mosh-style, in v1: paint time is ~1ms and network RTT
  is 30-150ms, so this is the only thing that makes a remote terminal feel local.
  A prediction is invisible until confirmed, so a phantom character is never
  shown ([ADR 0024](adr/0024-predictive-echo-in-v1.md)).
- **The performance budget is acceptance criteria**: keystroke to upstream under
  5ms, output to a live client under 20ms, zero steady-state allocations per
  frame, measured by a shipped harness
  ([ADR 0014](adr/0014-performance-budget-as-acceptance-criteria.md)).

### Documentation

| | |
|---|---|
| [`skill/SKILL.md`](../skill/SKILL.md) | the agent skill: every rule an agent follows when it shares one of your sessions |
| [`docs/api.md`](api.md) | the full public API — build a client from this alone |
| [`docs/adr/`](adr/) | 36 architecture decision records, and why each cost was accepted |
| [`docs/troubleshooting.md`](troubleshooting.md) | nine real failures from building this, and what `doctor` says for each |
| [`docs/ATTRIBUTION.md`](ATTRIBUTION.md) | prior art this design learned from |
| [`SPEC.md`](../SPEC.md) | the binding build contract |

---

## Building

```bash
./scripts/build.sh              # bin/herdr-expose for this machine
./scripts/build.sh --source     # same, but never fall back to a download
./scripts/build.sh --download   # never build; fetch the release binary
./scripts/build.sh --release    # darwin/linux x amd64/arm64 tarballs in dist/
./scripts/build.sh --skip-web   # Go only, reusing an existing web/dist
```

The web app is built first and embedded into the binary, so `go build ./...` on
its own will not work from a clean checkout until `web/dist` exists (`web/dist`
is gitignored, and is a build product in every checkout). Use the script.

With no flags the script builds from source when `go` and `npm` are on PATH, and
otherwise falls back to the release asset for your platform — which is what makes
`herdr plugin install` work on a machine with no toolchain. The fallback refuses
to install anything whose SHA-256 does not match the release's `SHA256SUMS`.

### Releases

`./scripts/build.sh --release` produces, for `darwin` and `linux` x `amd64` and
`arm64`:

```
dist/herdr-expose_<version>_<os>_<arch>.tar.gz
dist/herdr-expose_<version>_SHA256SUMS
```

`CGO_ENABLED=0` means one machine cross-compiles all four, and the version,
commit and build date are stamped in with `-ldflags`. Pushing a `vX.Y.Z` tag
runs [`.github/workflows/release.yml`](../.github/workflows/release.yml), which
runs `go test ./...`, builds that matrix and publishes it as a GitHub release
with `gh`. Nothing is signed or notarized, and the workflow does not pretend
otherwise.

---

## Troubleshooting

**The browser reconnects endlessly.** Almost always one of two things: a stale
`serve` process holding the port, or the `herdr` binary not being found inside a
forked child. `herdr-expose status` reports both. Under launchd or systemd the
`PATH` is minimal, which is why we resolve `herdr` to an absolute path — if that
resolution fails you get a real error naming what we looked for
([ADR 0020](adr/0020-absolute-path-herdr-binary.md)).

**It dies and does not come back.** The Herdr startup hook is one-shot and
unsupervised. Run `herdr-expose service install` for a launchd / systemd unit
with restart, and `herdr-expose service status` to confirm the manager picked it
up ([ADR 0019](adr/0019-three-layer-supervision.md)).

**A pane is blank on my Android phone.** The terminal renderer probes the canvas
after drawing and falls back to the DOM renderer when the surface comes back
dead, remembering the result. If it is still blank, file an issue with your
device and browser — that probe has a device list to grow.

**My `cloudflare = true` config stopped working.** It no longer does anything:
herdr-expose does not raise tunnels ([ADR 0038](adr/0038-no-tunnel-the-lan-is-the-top-rung.md)).
The daemon starts, serves loopback, and logs a warning naming the dead keys. To
get the hostname back, point a tunnel at the port yourself —
`cftunnel add 21118 herdr` — and set `lan = true` only if you also want the
wifi to reach it directly.

**My agent does not know about `herdr-share`.** Skills are discovered when an
agent session starts, so a skill installed mid-session is not visible until the
next one. If a new session still cannot see it, run `herdr-expose skill status`:
it says where the link is, whether it resolves, and whether it is pointing at an
old checkout.

**My settings keep reverting.** They should not: the config path is
`~/.config/herdr-expose/config.toml` regardless of how the process was started.
If you see otherwise, that is a bug worth reporting.

---

## License

MIT — see [`LICENSE`](../LICENSE). Prior art and what was learned from it:
[`docs/ATTRIBUTION.md`](ATTRIBUTION.md).
