# herdr-expose

Your [Herdr](https://herdr.dev) panes, in a browser or on your phone.

One Go binary. It connects to the Herdr socket on your machine, serves a
mobile-first web UI it has embedded inside itself, and — when you ask it to —
puts that UI behind a tunnel so you can check on your agents from anywhere.

```
Herdr  --unix socket-->  herdr-expose  --127.0.0.1:21118-->  browser
                              |
                              +--LAN 0.0.0.0:21118----->  your phone, same wifi
                              +--cloudflared tunnel---->  your phone, anywhere
                                 (static, your domain)
```

**The API is the product.** The web UI is its first client; a native mobile app
is meant to be its second, built against [`docs/api.md`](docs/api.md) with no
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

---

## Requirements

- Herdr **0.9.0** or newer (`herdr --version`)
- macOS or Linux
- Build time only: Go 1.22+ and node 20+. **Neither is needed at run time.**

---

## Install

### As a Herdr plugin (recommended)

```bash
herdr plugin install github.com/muthuishere/herdr-expose
```

That clones it, runs the build, and registers a startup hook so the server comes
up with Herdr. Then, from the TUI or the shell:

```bash
herdr plugin action invoke hex:open   # open the local UI
herdr plugin action invoke hex:pair   # show a pairing QR for a phone
herdr plugin action invoke hex:status   # what is running, and where
```

Every id is namespaced `hex:` because `herdr plugin action invoke` takes
`--plugin` as optional, so a bare `open` would collide with any other plugin
defining one. (`:` is a legal id character in Herdr 0.9.0; `.` is not.)

### From source

```bash
git clone https://github.com/muthuishere/herdr-expose
cd herdr-expose
./scripts/build.sh          # web app, then the Go binary with it embedded
./bin/herdr-expose serve    # http://127.0.0.1:21118
```

Link it into Herdr without installing from GitHub:

```bash
herdr plugin link .
```

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
| `status` | server state, local URL, tunnel URL, paired devices |
| `stop` | stop the running server |
| `pair` | mint a single-use pairing code and show a QR |
| `expose start\|stop\|status` | bring the tunnel up or down |
| `expose destroy` | remove the DNS record and delete the tunnel (explicit, and only what it created) |
| `share [--lan\|--quick\|--domain X\|--local]` | expose ONE session on a time-boxed, self-destructing URL (no flag = LAN) |
| `share list\|extend\|pair\|revoke\|restore` | manage shares (every verb takes `--json`) |
| `panic` | revoke every share and stop the main tunnel |
| `install-service` / `uninstall-service` | install or remove a launchd / systemd unit for crash recovery |

---

## Exposing it

Four modes. The mode decides the bind address — **`bind` is not a setting you
control**, which is deliberate, because this binary runs commands.

| mode | reachable from | device token | installable PWA |
|---|---|---|---|
| `local` (the **daemon's** default) | this machine only | not required | yes |
| `lan` (a **share's** default) | anyone on your wifi | **required** | **no** (see below) |
| `quick` | the internet, on a throwaway hostname | **required** | not usefully (see below) |
| `cloudflare` | the internet, on your domain | **required** | yes |

`local`, `lan` and `cloudflare` are what the long-lived server runs in. `quick`
belongs to [`share`](#sharing-one-session) alone and cannot be configured here.

The daemon defaults to `local` and a `share` defaults to `lan`, deliberately:
the daemon serves whoever is sitting at this machine, while a share exists to
be opened from somewhere else. Same principle — the least exposure that still
does the job — two different jobs. Nothing above those defaults is ever reached
without asking for it.

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
herdr-expose expose start
# http://192.168.1.24:21118   (also shown as a QR by `herdr-expose pair`)
```

This binds `0.0.0.0`, so **anyone on the network can reach the port** — the
server logs a warning saying so on every start. It is safe because a device token
is mandatory and **the pairing code is only ever shown on this machine's screen**;
no endpoint will hand one out. LAN mode is also the automatic fallback when
`cloudflare = true` is configured but `cloudflared` is not installed: it logs
loudly and falls back rather than leaving you with nothing.

> **No PWA install in lan mode.** Plain HTTP on a LAN IP is not a secure context
> (`localhost` is exempt; `192.168.x.x` is not), so the browser will not register
> a service worker or offer to install the app. `status` reports
> `secure_context: false`. You get a working web app in a browser tab, not a
> home-screen app. That is a rule of the web platform, not something we can fix.

### cloudflare — your own domain, static

A stable `https://` URL you can bookmark, install as a PWA, and bind device
tokens to.

```toml
[expose]
cloudflare  = true
domain      = "herdr.example.com"   # REQUIRED
tunnel_name = "herdr-expose"        # optional
```

```bash
export CLOUDFLARE_ALLPURPOSE_TOKEN=...
export CLOUDFLARE_ACCOUNT_ID=...
herdr-expose expose start
# https://herdr.example.com
```

That one command verifies your token's scopes, creates or reuses the named
tunnel, **writes the tunnel credentials itself from the API** (so there is no
`cloudflared login`, no browser, no interactive `cert.pem` — it works fine over
SSH), upserts the DNS CNAME, generates the ingress config, runs `cloudflared`,
**polls `/healthz` until the domain actually answers**, and restarts it if it
dies. The API token is read from the environment **by name, at point of use** —
never written to config, state, or a log.

`expose stop` stops `cloudflared` and leaves the tunnel and DNS in place, because
the domain is meant to be static. `expose destroy` is the explicit verb that
removes them, and it only ever touches records it created.

> **The permanent deployment is never ephemeral.** `cloudflare = true` without
> `domain` is a startup error, and there is no key that makes this server run on
> a `*.trycloudflare.com` hostname. A hostname that changes on every restart
> breaks PWA installs, bookmarks and origin-bound device tokens — which makes it
> worse than useless for the endpoint you open every day. A throwaway hostname
> is available where it actually fits: `herdr-expose share --quick`.

Needs `cloudflared` on your PATH (`brew install cloudflared`).

### quick — a throwaway public URL, for shares only

`herdr-expose share --quick` runs `cloudflared tunnel --url` and gets a random
`https://<words>.trycloudflare.com` hostname from Cloudflare's edge. **No
account, no zone, no DNS record, no API token is read** — the whole Cloudflare
API path is never entered — and there is nothing left to clean up afterwards:
the tunnel dies with the process. Measured cold start on a laptop: about ten
seconds from the command to a URL that answers.

The trade-off, which the CLI prints once at create time: **the hostname is new
every time.** Device tokens are origin-bound, so every quick share needs a fresh
pairing scan, and an installed PWA would be pinned to a hostname that stops
existing. Right for a throwaway, wrong for a daily driver — which is exactly why
`[expose]` keeps a static domain.

A quick tunnel is on the public internet, so it is **not** more trusted than a
domain share: same mandatory pairing, same server-side scope, same expiry.

### ngrok

Same static-domain rule — a reserved domain is required, no random URLs.

```toml
[expose]
ngrok  = true
domain = "herdr.ngrok.app"
```

Needs `ngrok` on your PATH and authenticated (`ngrok config add-authtoken ...`).

### Anything else: write an adapter

Tailscale funnel, a corporate reverse proxy, an SSH `-R`, your homelab. Adapters
are small JavaScript files run on an embedded Go JS runtime — **no node at run
time** — and they are the escape hatch, not the happy path. If you only want
Cloudflare or ngrok, you never touch this.

```js
// adapters/my-tunnel.js
export function start(ctx) {
  const p = ctx.spawn("my-tunnel", ["--port", "21118"]);
  ctx.onLine(p, (line) => {
    const m = /(https:\/\/\S+)/.exec(line);
    if (m) ctx.setUrl(m[1]);
  });
  return { pid: p.pid };
}

export function status(ctx) { return { url: ctx.url, healthy: ctx.isAlive() }; }
export function stop(ctx)   { ctx.kill(); }   // must be idempotent
```

```toml
[expose]
adapter = "my-tunnel"      # an adapter id selects the escape hatch

[[expose.adapters]]
id = "my-tunnel"
script = "adapters/my-tunnel.js"

[expose.adapters.env]        # free-form; arrives as ctx.config
region = "eu"
```

The host API on `ctx`:

| | |
|---|---|
| `spawn(cmd, args)` | start a process |
| `onLine(proc, fn)` | callback per line of its output |
| `setUrl(s)` / `url` | publish / read the public URL |
| `localUrl` | the loopback URL to tunnel to |
| `isAlive()` / `kill()` | process lifecycle |
| `log(s)` | write to the plugin log |
| `config` | this adapter's `env` table |
| `env(name)` | read a process env var **by name** — the value never reaches logs or state |

Adapter rules: **no filesystem access, no network from JS** (spawn a real tool
that does it properly), a crash never takes the server down, and `stop()` must be
safe to call twice. Copy `adapters/template.js` to start.

---

## Sharing one session

`expose` puts the whole machine's Herdr tree behind one long-lived URL. `share`
does the opposite: **one session, one URL, one deadline, then gone.**

```bash
herdr-expose share                          # http://<lan-ip>:<port>   THE DEFAULT
herdr-expose share --lan                    # the same, said out loud
herdr-expose share --quick                  # https://<random>.trycloudflare.com
herdr-expose share --domain x.example.com   # https://x.example.com    your zone
herdr-expose share --local                  # http://127.0.0.1:<port>  testing only
```

### The ladder is climbed, never guessed (AMENDMENTS 16)

| rung | flag | reach | needs | teardown at expiry |
|---|---|---|---|---|
| **lan** | **none** (default) | this machine **and this network** | nothing | stop the process, wipe the state |
| quick | `--quick` | anyone on the internet | `cloudflared` | stop the process, wipe the state (nothing exists in Cloudflare) |
| domain | `--domain X` | anyone on the internet, on a name you own | `cloudflared` + a zone your token reaches | the above **plus** delete the DNS record and the named tunnel |
| local | `--local` | this machine only — an opt-**in** for testing | nothing | stop the process, wipe the state |

The flags are mutually exclusive, and **no flag means LAN**. Never `--quick`,
never a configured domain. There is no auto-escalation: a bare `share` will not
put a session on the public internet because `[expose] domain` happens to be
set for the permanent deployment.

LAN is the default rather than loopback because a share exists to be **opened
from somewhere else** — at this machine you would just use the daemon on
`localhost:21118`. Binding `0.0.0.0` covers loopback and the wifi in one, and
goes no further. (The daemon's own `[expose]` default stays `local`. Same
principle — the least exposure that still does the job — different job.)

**Fallback only ever goes down.** An explicit `--quick` or `--domain` with no
`cloudflared` installed degrades to LAN and prints the reason, because you did
ask to be reachable and a LAN address is the nearest honest answer. Nothing
ever escalates above what you asked for.

Why the ceremony: this binary runs arbitrary commands inside your agent
sessions. The blast radius of each rung differs by orders of magnitude — this
machine, this room, the entire internet — and that difference must never be a
config key you forgot you set. Typing `--quick` takes a second and makes the
reach a conscious choice, which is the only thing that makes the pairing, scope
and TTL guarantees below mean anything.

The chosen rung and its reach are printed at create time (`exposure: lan —
anyone on this network`), so you never have to infer it from the URL.

Everything else is identical across all four: **scope is enforced in the
server** (the shared instance's tree contains only that session — other sessions
are absent from the tree, from subscribe, and rejected at the hub), **pairing is
mandatory**, and every share is time-boxed three ways (an in-process timer, a
token deadline, and a `share.json` the daemon sweeps). There is no permanent
share; a long one is `--days 30`.

```bash
herdr-expose share list [--json]            # also reaps anything past its deadline
herdr-expose share extend <id> --hours 2
herdr-expose share pair <id>                # another code for a share already running
herdr-expose share revoke <id> | --all      # immediate, idempotent, verified against the API
herdr-expose panic                          # every share down AND the main tunnel stopped
```

`revoke --all` and `panic` handle a mix of local + lan + quick + domain shares
in one pass; one rung's failure never aborts the others. Teardown reports per
component and exits non-zero if anything survived — for `local`, `lan` and
`quick` the DNS and tunnel lines read as not-applicable, because nothing was
ever created.

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
cloudflare  = false
domain      = ""             # REQUIRED when cloudflare = true
tunnel_name = "herdr-expose"
lan         = false          # also the automatic fallback if cloudflared is missing
autostart   = false

[share]
domain_suffix  = ""          # with default_mode = "domain": `share --name review` -> review.<suffix>
default_hours  = 1           # TTL when neither --hours nor --days is given
default_mode   = "lan"       # local | lan | quick | domain — the rung a bare `share` climbs to
max_concurrent = 10
```

`default_mode` ships as `"lan"` and is the only key that can move a bare
`share` off the LAN — setting it is asking, in the same sense that typing
`--quick` is asking. A `"auto"` written before AMENDMENTS 16 still loads and now
means `"lan"`. There is no `quick` key under `[expose]`, on purpose.

**There are no secrets in this file.** Tokens live as SHA-256 hashes in the state
directory at mode 0600; the Cloudflare token is read from the environment. See
[ADR 0009](docs/adr/0009-toml-config-outside-repo.md) and
[ADR 0017](docs/adr/0017-hashed-per-device-tokens.md).

---

## Security

**Read this part.**

`herdr-expose` talks to Herdr, and Herdr can run commands. `pane.run`,
`pane.send_text` and `agent.prompt` are, by design, **arbitrary command
execution as you, on your machine**. The moment a tunnel is up, this is a remote
code execution endpoint on the public internet.

That is not a scary edge case. It is the feature. A terminal you cannot type into
is a screenshot. So authentication here is the primary feature, not a checkbox.

### What the tool does about it

- **You do not choose the bind address; the mode does.** `local` and
  `cloudflare` bind `127.0.0.1`. `lan` binds `0.0.0.0` and says so loudly in the
  log on every start. There is no free-form `bind` setting to get wrong.
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

- **Do not leave a tunnel up.** `herdr-expose expose stop` when you are done. The
  server keeps running on loopback.
- **Do not leave `lan = true` on at a conference or a coffee shop.** It is meant
  for your own network.
- **Revoke devices you no longer use.** `herdr-expose status` lists them.
- **Your tunnel provider terminates TLS.** Cloudflare or ngrok can see the
  traffic. This is not end-to-end encrypted and nobody should imply otherwise.
- **Treat the URL as a credential** even though it is not one. A public URL plus
  an unpatched auth bug is a shell; do not paste it into a group chat.
- **Prefer a named tunnel with Cloudflare Access** in front of it if you are
  doing this from a work machine.
- **Adapters spawn processes.** An adapter you did not write is code you are
  choosing to trust.

Found a security problem? Open a private security advisory on the repository
rather than a public issue.

---

## How it works

Short version, with the details in the ADRs:

- **Transport and UI only.** Herdr does the work; we never reimplement it, never
  parse its TUI, never read its state files ([ADR 0002](docs/adr/0002-socket-api-is-the-only-contract.md)).
- Everything is the **socket API**, except terminal bytes, which Herdr 0.9.0 does
  not expose there — those come from the `herdr terminal session observe|control`
  subprocess ([ADR 0003](docs/adr/0003-terminal-streaming-via-subprocess.md)).
- **Split plane on one WebSocket**: JSON control frames, and binary data frames
  with an 11-byte header. No base64 on the wire, ever
  ([ADR 0006](docs/adr/0006-split-plane-json-control-binary-data.md)).
- **The server picks the mode** — live, summary or none — so a client cannot ask
  for twenty live terminals ([ADR 0007](docs/adr/0007-server-owned-viewport-modes.md)).
- **Predictive local echo**, Mosh-style, in v1: paint time is ~1ms and tunnel RTT
  is 30-150ms, so this is the only thing that makes a remote terminal feel local.
  A prediction is invisible until confirmed, so a phantom character is never
  shown ([ADR 0024](docs/adr/0024-predictive-echo-in-v1.md)).
- **The performance budget is acceptance criteria**: keystroke to upstream under
  5ms, output to a live client under 20ms, zero steady-state allocations per
  frame, measured by a shipped harness
  ([ADR 0014](docs/adr/0014-performance-budget-as-acceptance-criteria.md)).

### Documentation

| | |
|---|---|
| [`docs/api.md`](docs/api.md) | the full public API — build a client from this alone |
| [`docs/adr/`](docs/adr/) | 25 architecture decision records, and why each cost was accepted |
| [`docs/ATTRIBUTION.md`](docs/ATTRIBUTION.md) | prior art this design learned from |
| [`SPEC.md`](SPEC.md) | the binding build contract |

---

## Building

```bash
./scripts/build.sh              # bin/herdr-expose for this machine
./scripts/build.sh --release    # darwin/linux x amd64/arm64 tarballs in dist/
./scripts/build.sh --skip-web   # Go only, reusing an existing web/dist
```

The web app is built first and embedded into the binary, so `go build ./...` on
its own will not work from a clean checkout until `web/dist` exists. Use the
script.

---

## Troubleshooting

**The browser reconnects endlessly.** Almost always one of two things: a stale
`serve` process holding the port, or the `herdr` binary not being found inside a
forked child. `herdr-expose status` reports both. Under launchd or systemd the
`PATH` is minimal, which is why we resolve `herdr` to an absolute path — if that
resolution fails you get a real error naming what we looked for
([ADR 0020](docs/adr/0020-absolute-path-herdr-binary.md)).

**It dies and does not come back.** The Herdr startup hook is one-shot and
unsupervised. Run `herdr-expose install-service` for a launchd / systemd unit
with restart ([ADR 0019](docs/adr/0019-three-layer-supervision.md)).

**A pane is blank on my Android phone.** The terminal renderer probes the canvas
after drawing and falls back to the DOM renderer when the surface comes back
dead, remembering the result. If it is still blank, file an issue with your
device and browser — that probe has a device list to grow.

**`cloudflare = true` refuses to start.** You need `domain` set — there is no
ephemeral tunnel to fall back to, by design. Either set a domain, or use
`lan = true`.

**My settings keep reverting.** They should not: the config path is
`~/.config/herdr-expose/config.toml` regardless of how the process was started.
If you see otherwise, that is a bug worth reporting.

---

## License

MIT. See [`docs/ATTRIBUTION.md`](docs/ATTRIBUTION.md) for prior art.
