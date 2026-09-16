# herdr-expose

Your [Herdr](https://herdr.dev) panes, in a browser or on your phone.

One Go binary. It connects to the Herdr socket on your machine, serves a
mobile-first web UI it has embedded inside itself, and — when you ask it to —
puts that UI behind a tunnel so you can check on your agents from anywhere.

```
Herdr  --unix socket-->  herdr-expose  --127.0.0.1:21118-->  browser
                              |
                              +--cloudflared/ngrok-->  your phone
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
  handling. Online only, on purpose — a cached terminal is a lie.
- **Loopback by default.** Remote access is a tunnel you start, and stop.

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
| `install-service` | install a launchd / systemd unit for crash recovery |

---

## Exposing it

The server **never** binds anything but `127.0.0.1`. Remote access is always a
tunnel you start. Three ways, in order of how much setup they need.

### Cloudflare quick tunnel — zero setup

A random `*.trycloudflare.com` URL, no account, no DNS. Good for "let me check on
this from the train".

```toml
# ~/.config/herdr-expose/config.toml
[expose]
enabled = true
cloudflare = true      # no domain => quick tunnel
```

```bash
herdr-expose expose start
# https://ancient-violet-bread-9f2x.trycloudflare.com
```

Needs `cloudflared` on your PATH (`brew install cloudflared`).

### Cloudflare named tunnel — your own domain

A stable URL you can bookmark and install as a PWA.

```toml
[expose]
enabled = true
cloudflare = true
domain = "herdr.example.com"
```

```bash
export CLOUDFLARE_ALLPURPOSE_TOKEN=...   # or whatever you have named it
herdr-expose expose start
# https://herdr.example.com
```

That one command creates or reuses the named tunnel, writes the DNS CNAME
through the Cloudflare API, runs `cloudflared`, health-checks it, and restarts it
if it dies. The API token is read **from the environment by name, at point of
use** — it is never written to the config file, the state directory, or a log.

### ngrok

```toml
[expose]
enabled = true
ngrok = true
# domain = "herdr.ngrok.app"   # optional, with a reserved domain
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
enabled = true
adapter = "my-tunnel"

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

## Configuration

`~/.config/herdr-expose/config.toml`, always — the path does not change depending
on whether Herdr or your shell started the process. Created with defaults on
first run. Reloaded on `SIGHUP`. Unknown keys are preserved, so a config written
by a newer version survives an older binary.

```toml
[server]
port = 21118
bind = "127.0.0.1"           # any other value is refused at startup

[auth]
pairing_ttl_seconds = 600    # 10 minutes
device_ttl_days = 30         # sliding, from last use
max_devices = 32

[ui]
theme = "auto"
default_view = "grid"        # grid | focus

[expose]
enabled = false
cloudflare = false
ngrok = false
# domain = "herdr.example.com"
autostart = false
```

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

- **Binds `127.0.0.1` only.** Setting `bind` to anything else is **refused with
  an error**, not warned about. There is no flag to expose it directly to your
  network; a leaked token does not also hand you to everyone on the coffee-shop
  wifi.
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
- **The performance budget is acceptance criteria**: keystroke to upstream under
  5ms, output to a live client under 20ms, zero steady-state allocations per
  frame, measured by a shipped harness
  ([ADR 0014](docs/adr/0014-performance-budget-as-acceptance-criteria.md)).

### Documentation

| | |
|---|---|
| [`docs/api.md`](docs/api.md) | the full public API — build a client from this alone |
| [`docs/adr/`](docs/adr/) | 20 architecture decision records, and why each cost was accepted |
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

**My settings keep reverting.** They should not: the config path is
`~/.config/herdr-expose/config.toml` regardless of how the process was started.
If you see otherwise, that is a bug worth reporting.

---

## License

MIT. See [`docs/ATTRIBUTION.md`](docs/ATTRIBUTION.md) for prior art.
