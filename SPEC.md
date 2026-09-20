# herdr-expose — build contract

ONE Go binary: Herdr socket -> WebSocket -> embedded React PWA, exposed through
pluggable JS tunnel adapters. Verified against **Herdr 0.9.0, protocol 22**.

Read `references/architecture.md` for the full rationale. This file is the
CONTRACT between parallel workstreams — where it disagrees with architecture.md,
this file wins (architecture.md predates the 0.9.0 verification).

Module: `github.com/muthuishere/herdr-expose`   Binary: `bin/herdr-expose`

## 0. Verified upstream facts (do not re-derive)

- Herdr 0.9.0, protocol 22, endpoint_protocol_generation 1.
- Socket API: **102 request methods**, 26 events. Schema: `herdr api schema --output x.json`.
- **There is NO `terminal.*` method on the socket API.** Terminal streaming is ONLY
  the subprocess `herdr terminal session observe|control <target> --cols N --rows N`,
  NDJSON on stdout: `{"bytes":"<base64 ANSI>"}` and `{"type":"terminal.closed","reason":...}`.
  Control accepts terminal.input / resize / scroll / release as NDJSON on stdin.
- **Subscribe BEFORE snapshot.** 0.9.0 stopped replaying retained history on
  `events.subscribe`; snapshot-then-subscribe silently loses the gap.
- Useful 0.9.0-only methods: `pane.scroll`, `pane.selection.read`, `pane.copy_search`,
  `pane.copy_motion`, `pane.link.activate`, `command.invoke`.
- Env given to plugin commands: `HERDR_SOCKET_PATH`, `HERDR_BIN_PATH`, `HERDR_PLUGIN_ROOT`,
  `HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_EVENT_JSON`.
- Pane IDs are NOT stable across server restarts. `terminal_id` is stable across moves.

## 1. Hard rules

1. Never reimplement Herdr. Transport + UI only.
2. Bind `127.0.0.1` ONLY. Never 0.0.0.0. Exposure is always an adapter.
3. One upstream stream per target regardless of client count. The hub fans out.
4. Decode base64 once, in the upstream layer. Everything downstream is binary.
5. Server owns layout/state/mode. Clients render what they are told.
6. No secret literal in any file in this repo. Token is generated at runtime.
7. Mobile-first: every view must work at 375px wide before it works at 1440px.

## 2. Ownership map (no workstream writes outside its column)

| workstream | owns |
|---|---|
| **A · go-core**    | `cmd/`, `internal/upstream/`, `internal/core/`, `internal/serve/`, `go.mod` |
| **B · web**        | `web/` |
| **C · expose**     | `internal/expose/`, `adapters/`, `internal/config/` |
| **D · docs/pkg**   | `docs/`, `herdr-plugin.toml`, `scripts/`, `README.md`, `.gitignore` |

## 3. Wire protocol (A defines, B consumes) — JSON over WS, v1

> **PARTLY SUPERSEDED** by AMENDMENTS 1 (A1 split plane) and 2 (B2 resize, B4 no resume). See D1 below.

`GET /v1/stream?token=...` (also accepts `Authorization: Bearer`). Every frame:
`{"seq":<uint64>,"type":"<name>","data":{...}}`. seq is monotonic per connection.

client->server: `hello` `subscribe` `unsubscribe` `viewport` `input` `command` `ping`
server->client: `welcome` `tree` `frame` `closed` `snapshot` `gap` `agent` `result` `pong`

- `viewport`: `{"targets":{"<paneId>":"live|summary|none"}}` — client declares what it
  RENDERS; server decides the real mode. Clients never request live directly.
- `frame`: `{"target":..,"bytes":"<base64>","seq":..}` (base64 only at the JSON boundary).
- `gap`: `{"target":..,"bytes_dropped":N}` then a `snapshot`. Never buffer unboundedly.
- `command`: `{"method":"pane.send_text","params":{...},"id":".."}` -> `result`.
  Method names pass straight through to the Herdr socket. Do not enumerate them.

Modes: LIVE = focused, full stream, 16ms coalescing / 64KB flush.
SUMMARY = visible-unfocused, `pane.read --source visible` at 1-2Hz.
NONE = offscreen, state changes only.

## 4. HTTP surface

```
GET  /healthz     unauthenticated liveness
GET  /v1/config   client bootstrap (no secrets)
POST /v1/pair     one-time token -> long-lived key
GET  /v1/stream   websocket
GET  /*           embedded SPA (go:embed all:web/dist)
```
Hashed assets immutable; `index.html` + `sw.js` no-cache.

## 5. Config — TOML, `$HERDR_PLUGIN_CONFIG_DIR/config.toml`, else `~/.config/herdr-expose/config.toml`

> **SUPERSEDED** by AMENDMENTS 2 (B5 auth, B6 path) and 3 (C1 static domain). Canonical block is D2 below.

Created with sane defaults on first run if absent. Hot-reload on SIGHUP.

```toml
[server]
port = 21118
bind = "127.0.0.1"          # changing this is refused with an error; loopback only

[auth]
token = ""                   # blank => generated on first run and written back, 0600
pairing_ttl_seconds = 60

[ui]
theme = "auto"
default_view = "grid"        # grid | focus

[expose]
enabled = false
adapter = "cloudflare-quick" # id of an adapter below
autostart = false

[[expose.adapters]]
id = "cloudflare-quick"
script = "adapters/cloudflare-quick.js"
[expose.adapters.env]        # free-form, passed to the adapter as ctx.config
# hostname = "herdr.example.com"
```

Keys added later must not break older configs: unknown keys are ignored, missing
keys take defaults. Never write the token to logs or to `/v1/config`.

## 6. Exposure adapters — JS on embedded goja (workstream C)

> **SUPERSEDED** by AMENDMENTS 3. Cloudflare is built in; JS adapters are the escape hatch only.

An adapter is a JS file exporting three functions. The Go side runs it in goja;
NO node at runtime. Ship `cloudflare-quick`, `cloudflare-named`, `ngrok`, plus
a `template.js` people copy.

```js
// adapters/cloudflare-quick.js
export function start(ctx) {
  const p = ctx.spawn("cloudflared", ["tunnel", "--url", ctx.localUrl]);
  ctx.onLine(p, (line) => {
    const m = /(https:\/\/[a-z0-9-]+\.trycloudflare\.com)/.exec(line);
    if (m) ctx.setUrl(m[1]);
  });
  return { pid: p.pid };
}
export function status(ctx) { return { url: ctx.url, healthy: ctx.isAlive() }; }
export function stop(ctx)   { ctx.kill(); }
```

Host API on `ctx`: `spawn(cmd,args)` `onLine(proc,fn)` `setUrl(s)` `url` `localUrl`
`isAlive()` `kill()` `log(s)` `config` (the adapter's `env` table) `env(name)`
(reads a process env var by NAME; the value never crosses into logs or state).

Adapter rules: no filesystem access, no network from JS (spawn a real tool),
crash in an adapter never takes down the server, `stop()` must be idempotent.
A running adapter's URL is surfaced in the UI and by `herdr-expose status`.

## 7. CLI

> **EXTENDED** by AMENDMENTS 2 (B7 supervision) and D3 below.

`serve` (real server, flock on pidfile, quiet exit if held) · `daemon` (fork-exec
serve, exit 0 immediately — startup hooks are one-shot, not supervised) ·
`status` · `stop` · `pair` (QR) · `expose start|stop|status`.

## 8. Web (workstream B) — React + Vite + TS, PWA, MOBILE-FIRST

- **375px is the design target.** Single-column pane list -> tap a pane -> full-screen
  terminal with a key bar. Desktop grid is a `@media (min-width:900px)` enhancement.
  Test at 375 before 1440. No horizontal page scroll, ever.
- xterm.js ONLY for the LIVE pane. SUMMARY tiles are pre-rendered ANSI-to-HTML,
  never an xterm instance per tile — that is what kills the tab.
- PWA: `vite-plugin-pwa`, installable, standalone display, maskable icon, offline
  shell ONLY (app chrome + "disconnected" state). Never cache `/v1/*`. A terminal
  is inherently online; do not pretend otherwise.
- Reconnect with backoff, resume from last seq, show RTT-based degraded indicator.
- Agent Q&A view when an agent is `blocked`: `agent.read --source detection` plus a
  fixed key bar (y / n / enter / esc / arrows / 1 2 3). Generic, no per-agent parsing.
- Safe-area insets for iOS notch; the key bar sits above the home indicator.

## 9. Definition of done per workstream

A: `go build ./...` clean; `herdr-expose serve` starts, `/healthz` 200, a WS client
   gets `welcome` + `tree`, one pane streams frames, a keystroke round-trips.
B: `npm run build` clean; renders tree from a mock WS; Lighthouse PWA installable;
   usable at 375px.
C: adapters load and run under goja; `expose start` brings up cloudflared and
   reports a URL; `stop` is idempotent; config round-trips with unknown keys preserved.
D: ADRs written; `herdr-plugin.toml` validates via `herdr plugin link .`; build
   script produces one binary with the web app inside it.

---

# AMENDMENTS — authoritative, SUPERSEDE everything above

Reason: the reference implementation (`~/Downloads/herdr-remote-master`) is Node.
Mine it for FACTS about Herdr and for mobile gotchas. Do NOT inherit its
architecture. It makes JS-shaped choices we are explicitly not making.
**This is a Go, performance-first product. That is the differentiator.**

## A1. Split plane: JSON control, BINARY data (replaces §3's framing)

Carrying terminal bytes as base64 inside JSON costs +33% bandwidth and a JSON
parse per frame, on the hot path, over cellular. We do not do that.

- **Control plane** — JSON in WebSocket TEXT frames. hello/subscribe/unsubscribe/
  viewport/command/ping and welcome/tree/agent/result/pong/gap/closed. Unchanged
  shape from §3, minus `frame`.
- **Data plane** — WebSocket BINARY frames. No base64, anywhere, ever:

```
 0        1                9          11                    N
 +--------+----------------+----------+----------+----------+
 | type u8| seq u64 BE     | tlen u16 | target   | payload  |
 +--------+----------------+----------+----------+----------+
 type: 1=frame 2=snapshot 3=gap(payload=u64 bytes_dropped)
```

`target` is ASCII. `payload` is RAW ANSI bytes straight from the pane. Base64
exists at exactly ONE place in this system: decoding Herdr's NDJSON in
internal/upstream. After that it is `[]byte` to the socket, zero re-encoding.

Client input is also binary: `[type=16][tlen u16][target][raw bytes]`. Keystroke
latency is the metric we care about; do not put a JSON encode on that path.

Enable per-message deflate. Terminal output compresses ~10x and that is what
makes a tunnel usable on cellular.

## A2. Cloudflare is FIRST-CLASS, not a JS adapter (replaces §6's default path)

The common case must be two lines of config and zero JavaScript:

```toml
[expose]
cloudflare = true
domain = "herdr.deemwar.com"   # omit => quick tunnel, random *.trycloudflare.com
```

That alone must: create/reuse the named tunnel, write the DNS CNAME through the
Cloudflare API, run cloudflared, health-check it, restart it if it dies, and
surface the URL in the UI and in `herdr-expose status`. Built in Go, in-process.
`ngrok = true` likewise. Token read from `$CLOUDFLARE_ALLPURPOSE_TOKEN` by NAME
at point of use — never stored, never logged, never in config, never in state.

JS adapters (goja) REMAIN, but demote them to the escape hatch for exotic setups
(tailscale, custom corporate proxy, someone's homelab). They are the extension
point, not the happy path. Nobody should write JS to put this on a domain.

## A3. Performance budget — these are acceptance criteria, not aspirations

> **SUPERSEDED in its memory row** by AMENDMENTS 20: the RSS line measured only
> the server process and ignored the `herdr terminal session observe`
> subprocesses the design mandates. N20 below carries the measured numbers.

| path                          | target                          |
|-------------------------------|---------------------------------|
| keystroke -> upstream write   | < 5ms                           |
| output -> LIVE client (LAN)   | < 20ms including coalescing     |
| allocations per terminal frame| ZERO steady-state (pool buffers)|
| idle CPU, 20 panes            | < 1%                            |
| RSS, 20 panes                 | < 60MB                          |

Implied and non-negotiable: pooled buffers on the fanout path (`sync.Pool`),
one upstream stream per target no matter how many clients, coalesce on a ~16ms
tick or 64KB, and never a per-client copy of a frame that could be shared.
Ship a latency harness that MEASURES these; a claimed number is not a number.

## A4. The API is the product

A Swift/Kotlin client must be buildable from `docs/api.md` alone, with no access
to this source. The binary framing above is deliberately trivial to parse in any
language — that is why it is a fixed header and not protobuf. Version the API and
state the compatibility promise explicitly.

---

# AMENDMENTS 2 — from recon of the Node reference (MIT, dibin666/herdr-remote)

Supersede everything above, including AMENDMENTS 1 where they conflict.

## B0. SETTLED: `herdr terminal session observe` DOES exist and works

Recon flagged §0 as unverified because the reference never calls that subcommand.
**§0 stands.** It was verified empirically on this machine on BOTH 0.8.2 and 0.9.0:
`herdr terminal session observe <pane> --cols 80 --rows 24` returns NDJSON
`{"bytes":"<base64>"}` frames. The reference simply chose a DIFFERENT PRODUCT:
they spawn the whole Herdr TUI in a node-pty and stream its raw ANSI, so the
browser sees Herdr's own UI. That gets them real PTY semantics for free but
forfeits the pane tree, summary tiles and a custom mobile layout — and costs one
Herdr client process per browser tab. We keep the pane-tree product. Do not churn.

## B1. Per-connection streams (REPLACES hard rule #3)

Rule #3 said one upstream stream per target, hub fans out. **That is wrong on
0.9.0** and it reintroduces the exact bug 0.9.0's #3526 fixed: a shared stream
means a phone at 40 cols resizes the laptop looking at the same pane. Give each
CONNECTION its own upstream stream and its own geometry. The hub still
deduplicates SUMMARY reads (those are geometry-free), but LIVE is per-connection.

## B2. Geometry message (FIXES an omission — we had none)

There was no resize anywhere in §3; `viewport` is render-mode, not size. Add to
the control plane:
`{"type":"resize","data":{"target":"w1:p1","cols":N,"rows":M}}`
Floor at 20x6. Geometry is per-connection (see B1) and must be sent before the
first frame is requested. A terminal without a geometry message does not work.

## B3. Same-tick coalescing (REPLACES "16ms coalescing")

A 16ms timer adds up to 16ms to EVERY keystroke echo, which contradicts our own
<5ms budget. Coalesce on the tick instead: drain everything already queued, write
once, never sleep to accumulate. 64KB hard flush stays. Zero added latency, same
syscall savings. Input side: coalesce only what is already pending, never delay.

## B4. Resume is deleted (REPLACES seq/gap/snapshot replay)

Dropped: the per-target 256KB ring buffer and seq-based replay. On reconnect a
client re-subscribes, gets a fresh `snapshot`, and repaints. A terminal has no
perceivable "missed bytes" once you repaint. `seq` stays in the binary header for
ordering and debugging; `gap` stays as a signal under backpressure. The ring
buffer and replay path are cut entirely — that is correctness surface we do not
need to own.

## B5. Auth overhaul (REPLACES §4/§5 auth — weakest part of the spec)

This is an RCE surface behind a tunnel. A single plaintext long-lived token in a
TOML file is not good enough.
- Two secrets, separate concerns: a **server token** (may you talk to this
  instance at all) and **per-device tokens** (issued by pairing).
- **Store SHA-256 hashes only**, never the plaintext, in state (0600), not config.
- Pairing: 6-char code, **TTL 10 minutes** (60s is hostile on a phone), single
  use, rate-limited per address. Device token = 32 random bytes, sliding 30-day
  TTL, max 32 devices, LRU evict, individually revocable and listable
  (UA + last-seen IP, never the hash).
- `crypto/subtle.ConstantTimeCompare` for every comparison. No exceptions.
- Origin allowlist checked BEFORE echoing CORS headers. CSP `frame-ancestors
  'none'`, `X-Frame-Options: DENY`. Handshake rate limiting on the WS endpoint.

## B6. One config path (REPLACES §5 path resolution)

Do NOT prefer `$HERDR_PLUGIN_CONFIG_DIR`. Herdr sets it only when Herdr launches
us, so honoring it gives the tool two different configs depending on whether it
was started from a shell or from a Herdr pane — a genuinely confusing bug the
reference hit and documented. Use `~/.config/herdr-expose/config.toml`
unconditionally. `$HERDR_PLUGIN_STATE_DIR` is still fine for the pidfile.

## B7. Supervision is three layers, not one (EXTENDS §7)

`daemon` fork-exec + exit 0 is only step 1. Also required:
2. Install a real service unit — launchd LaunchAgent (`KeepAlive`) on macOS,
   `systemd --user` with `Restart=always` on Linux. `serve` runs in the
   foreground as that unit's main process.
3. An append-only managed-pid ledger + `reclaimStrays()` on takeover: SIGTERM
   every recorded pid, WAIT for the port to actually be released, then SIGKILL.
   Route every start/stop/restart through a "is a manager in charge?" check —
   otherwise killing the process just makes the manager respawn it, and a manual
   start produces a second copy fighting for the port.
Also: resolve the `herdr` binary to an ABSOLUTE path before any spawn
(`$HERDR_BIN_PATH` -> PATH -> ~/.local/bin, ~/.cargo/bin, ~/bin,
/opt/homebrew/bin, /usr/local/bin, /usr/bin), re-verified per session, and fail
with a real error rather than looping. launchd/systemd units start with a minimal
PATH that contains none of those dirs, so a bare `herdr` will fail exec.

## B8. Mobile is engineering, not a media query (EXTENDS §8)

"Mobile-first at 375px" is a layout goal. The actual work, all verified pain in
the reference — budget for it:
- **Renderer probe-and-degrade, not a config flag.** Coarse pointer => mount
  Canvas FIRST (mobile WebGL loses context / renders blank on some drivers).
  Then AFTER content is drawn (probing an empty buffer is a guaranteed false
  positive) sample pixels: all-identical => dead surface => dispose, fall back to
  the DOM renderer, and PERSIST that failure in localStorage keyed by UA with a
  30-day TTL. Re-verify after the first resize. Without this we ship a blank
  black rectangle on some Android devices.
- **Never `transform: scale()` the xterm surface.** xterm resolves a cell from
  the UNSCALED CSS cell width, so a scaled surface offsets every mouse report,
  selection and link hit-test. Recompute cols/rows from measured cell metrics.
- **Cell-height fallback ratio 1.30, not the configured lineHeight 1.15.** xterm
  ceils the font line box; using bare lineHeight yields ~13% too many rows and
  pushes agent output into scrollback. Err high.
- **Soft keyboard: only `visualViewport.height` shrinks** (100dvh and
  window.innerHeight both lie). Drive `--app-height` / `--keyboard-inset` from
  it, 80px threshold to reject browser-chrome noise, coalesce in rAF (iOS fires a
  burst for the whole keyboard animation).
- **Browser zoom must never resize the PTY.** Keep PTY geometry independent of
  client-local fontSize.
- **Reimplement touch; do not use xterm's.** Tap => replay through xterm's CORE
  MOUSE SERVICE, never a DOM mousedown (that focuses the hidden textarea and
  summons the IME on a stray tap). Vertical drag => scrollback on the normal
  buffer, synthesized wheel/arrows on the alternate screen. Long-press consumed,
  not a selection. Never pointer-capture a touch pointer. Pass real mouse through
  untouched.
- **Key bar**: ESC TAB CTRL ALT arrows, then drawers (ctrl-chords, symbols,
  F-keys), Enter LAST and rightmost under the thumb. Modifiers are LATCHES.
- **Mobile shell selection is width-only** (900px breakpoint), evaluated
  synchronously on first render. Pointer-coarse is the wrong signal: desktop-mode
  phones and touch laptops report a fine pointer.
- xterm: `scrollback: 5000`, `convertEol: true`, `allowProposedApi: true`,
  Unicode11Addon, and NO client theme / NO minimumContrastRatio — let every
  SGR/OSC colour through exactly as the host sent it.

## B9. Compression tuning (EXTENDS A1)

> **Backpressure clause CORRECTED** by AMENDMENTS 19: there is no `gap`-and-degrade
> ladder for a slow socket. Scroll shedding is real, but a client that cannot drain
> is disconnected. See N19 below for what was measured.

permessage-deflate `threshold: 1024` so keystrokes and small echoes skip
compression entirely (zero CPU, zero buffering delay), level 3, and leave context
takeover ON — full-screen TUI redraws are highly repetitive and cross-message
context is where the 10x ratio comes from.
Backpressure: if the socket's buffered amount exceeds ~256KB, drop WHEEL/scroll
input but never keystrokes. Degrade scrolling, never typing.

## B10. Predictive echo — phase 2, but reserve for it now

Mosh-style local echo is what makes a 150ms tunnel feel local. Predictions start
TENTATIVE and INVISIBLE; only after one is confirmed by real server output do
they go CONFIDENT and render; any mismatch wipes all pending predictions and
drops back to tentative, so a phantom character is never shown. Not required for
v1, but do not design anything that forecloses it.

## B11. Attribution

The reference is MIT (c) 2026 dibin666. We reimplement in Go, we do not copy
code, but we lift protocol and mobile ideas liberally. Ship `docs/ATTRIBUTION.md`
crediting it plainly.

---

# AMENDMENTS 3 — STATIC DOMAIN ONLY (supersedes A2 and all prior expose text)

Owner's direction, verbatim intent: the tunnel is STATIC, on HIS domain, built
from Go using the Cloudflare token + the cloudflared CLI + the domain name.

## C1. There is no ephemeral tunnel. At all.

`cloudflare = true` REQUIRES `domain`. No domain => hard error at startup with a
message telling the user to set one. **Delete the quick-tunnel path entirely** —
no `*.trycloudflare.com`, no random hostnames, not even as a fallback. A hostname
that changes on restart breaks PWA installs, bookmarks and origin-bound device
tokens, which makes it worse than useless for the mobile product.

```toml
[expose]
cloudflare = true
domain     = "herdr.deemwar.com"   # REQUIRED
tunnel_name = "herdr-expose"        # optional, defaults to "herdr-expose"
```

## C2. Full API-driven provisioning in Go — no `cloudflared login`

This is the key move: `cloudflared tunnel login` opens a BROWSER and writes an
interactive `cert.pem`. We never do that. With an API token we mint the tunnel
and its credentials ourselves, so the whole thing is headless and reproducible.

Inputs: `$CLOUDFLARE_ALLPURPOSE_TOKEN` (read by NAME at point of use, never
stored/logged/returned), `$CLOUDFLARE_ACCOUNT_ID`, and `domain` from config.

`expose start` is idempotent and does exactly this:

1. **Zone** — `GET /zones?name=<apex of domain>` -> `zone_id`.
2. **Tunnel** — `GET /accounts/{acct}/cfd_tunnel?name=<tunnel_name>&is_deleted=false`.
   Reuse if present. Else `POST /accounts/{acct}/cfd_tunnel` with
   `{name, tunnel_secret: <32 random bytes, base64>, config_src:"local"}` -> `id`.
3. **Credentials file** — write ourselves, 0600, into the state dir:
   `{"AccountTag":<acct>,"TunnelID":<id>,"TunnelSecret":<base64 secret>}`.
   This is the file `cloudflared login` would have produced. We produce it from
   the API instead. Never in the repo, never in config, never logged.
4. **DNS** — upsert CNAME: look up `GET /zones/{zone}/dns_records?name=<domain>`;
   PATCH if it exists, POST if not:
   `{type:"CNAME", name:<domain>, content:"<tunnel-id>.cfargotunnel.com",
     proxied:true}`. Never delete a record we did not create.
5. **Ingress config** — generate a `config.yml` in the state dir:
   `tunnel: <id>` / `credentials-file: <path>` / `ingress:
   [{hostname:<domain>, service:"http://127.0.0.1:<port>"},
    {service:"http_status:404"}]`
6. **Run** — `cloudflared tunnel --config <path> run <id>`, resolved to an
   absolute binary path first (same rule as the herdr binary, B7).
7. **Verify** — poll `https://<domain>/healthz` until 200 or timeout. Report the
   real URL only after it actually answers. A process that started is not a
   tunnel that works.
8. **Supervise** — restart on crash with backoff; surface state in
   `herdr-expose status` and in the UI.

`expose stop` kills cloudflared and leaves the tunnel + DNS record in place (it
is a STATIC domain — tearing down DNS on every stop is the ephemeral behaviour we
just deleted). A separate explicit `expose destroy` removes the DNS record and
deletes the tunnel, and only ever touches records it created.

## C3. Preflight before touching anything

Verify the token works and has the needed scopes BEFORE provisioning:
`GET /user/tokens/verify`, then confirm zone read + DNS edit on the target zone.
Fail with a precise, actionable error naming the missing permission. Never
half-provision: if DNS fails, do not leave a dangling tunnel.

## C4. ngrok and JS adapters

> **SUPERSEDED in part by AMENDMENTS 18.** The built-in ngrok provider is gone;
> the static-domain rule below stands for Cloudflare. JS adapters are no longer
> "for exotic setups only" — they are the extension point for every transport
> that is not built in, which since AMENDMENTS 18 means every transport but
> Cloudflare.

`ngrok = true` follows the same static-domain rule: a reserved domain is
required, no random URLs. goja/JS adapters remain the escape hatch for exotic
setups only. Neither is the happy path.

---

# AMENDMENTS 4 — resolving ambiguities found during packaging

## D1. `gap` and `snapshot` are BINARY-ONLY. Settled.

A1 listed `gap` in the control plane AND defined binary `type:3`. That was my
error. Both `snapshot` (type 2) and `gap` (type 3) are **binary frames only**,
never JSON. Reason: both must stay strictly ORDERED against the output stream
they refer to. A `gap` that arrives out of order relative to the bytes it
describes is worse than no gap at all. Remove them from the control-plane list.

Control plane (JSON text) is exactly:
  out: hello · subscribe · unsubscribe · viewport · resize · command · ping
  in:  welcome · tree · agent · closed · result · pong
Data plane (binary) is exactly:
  in:  1=frame · 2=snapshot · 3=gap        out: 16=input

## D2. Canonical config — this block replaces §5's

```toml
[server]
port = 21118
# no `bind` key: loopback is not configurable. It is enforced in code.

[auth]
pairing_ttl_seconds = 600     # 10 minutes. No token key — secrets live in
                              # state as SHA-256 hashes, never in config.

[ui]
theme = "auto"
default_view = "grid"

[expose]
cloudflare  = false
domain      = ""              # REQUIRED when cloudflare = true
tunnel_name = "herdr-expose"
autostart   = false
```
Path: `~/.config/herdr-expose/config.toml`, unconditionally (B6).
Unknown keys preserved on rewrite; missing keys take defaults.

## D3. CLI gains `install-service` / `uninstall-service`

B7 requires a launchd/systemd unit but §7 had no verb for it. Add:
`herdr-expose install-service` (writes and loads the LaunchAgent / systemd --user
unit, idempotent) and `uninstall-service`. `daemon` stays as the one-shot-hook
entry point; `serve` stays as the foreground process the unit supervises.

## D4. Manifest facts verified against live Herdr 0.9.0 (do not re-derive)

- `[[panes]]` requires id/title/command. `placement` ∈ overlay|popup|split|tab|zoomed.
- **`width`/`height` are POPUP-ONLY.** Setting them with `placement="overlay"`
  is rejected: `invalid_plugin_pane_size`. (Hit for real; overlay takes the whole
  terminal area anyway.) They accept a cell count or a "NN%" string.
- `[[link_handlers]]` requires id/title/pattern/action, where `action` is an
  action id. The invoked command receives `clicked_url` and `link_handler_id` in
  its invocation context.
- `[[actions]]` also accepts `contexts` ∈ global|workspace|tab|pane|selection.

---

# AMENDMENTS 5 — LAN mode (RELAXES hard rule #2, deliberately)

Owner's direction: "if cloudflared is not there let it expose everywhere so
people in wifi and all they can connect."

## E1. Three exposure modes, resolved in this order

1. **cloudflare** — `cloudflare = true` AND `domain` set AND the cloudflared
   binary is resolvable. Public, static domain. Binds 127.0.0.1; the tunnel is
   the only remote path.
2. **lan** — bind `0.0.0.0`, reachable by anyone on the wifi/LAN. Chosen when
   `lan = true`, OR AUTOMATICALLY when cloudflare was requested but **cloudflared
   is not installed** — log loudly, fall back, do not fail.
3. **local** — bind 127.0.0.1. The default when nothing else is configured.

```toml
[expose]
cloudflare = true
domain     = "herdr.deemwar.com"
lan        = true    # allow LAN; also the automatic fallback when cloudflared is absent
```

`bind` is no longer user-settable — the MODE decides it. Keep the key rejecting
manual values with a message pointing at `lan`.

## E2. Why this is safe: auth is mandatory, and the QR is local-only

Hard rule #2 said loopback only, because this binary executes arbitrary commands.
LAN mode is acceptable ONLY because of the pairing design (E3 in AMENDMENTS 6 /
the local-only QR):

- **Auth is never optional. There is no "trusted LAN" bypass.** A device token is
  required in every mode, including local.
- **The pairing code is displayed ONLY on the physically-present machine** and no
  HTTP endpoint ever mints or shows one. So someone on the same wifi can reach
  the port, complete a TLS/WS handshake, and get exactly nowhere: they cannot
  obtain a pairing code without looking at your screen.
- Therefore the LAN exposure surface is: an unauthenticated `/healthz`, a static
  SPA, and a hard rate-limited `POST /v1/pair` that only ACCEPTS codes.

Log a clear one-line warning on every LAN-mode start naming the bind address and
reminding that anyone on the network can reach the port.

## E3. LAN mechanics

- Resolve the primary non-loopback IPv4 at startup; show it in `status`, in the
  UI, and encode it in the QR (`http://<lan-ip>:<port>/?pair=<code>`).
- The QR URL is chosen by MODE: cloudflare -> `https://<domain>`, lan ->
  `http://<lan-ip>:<port>`, local -> `http://127.0.0.1:<port>`.
- Re-resolve the IP on SIGHUP and on network change; a DHCP lease change must not
  silently leave a stale URL in `status`.
- Origin allowlist must accept the LAN origin in lan mode, or the browser will
  refuse the WebSocket.
- **Plain HTTP on LAN means no service worker and no PWA install** (both require
  a secure context; `localhost` is exempt, a LAN IP is not). Say so plainly in
  status and in the README — on LAN the phone gets a working web app but not an
  installable one. That is a real limitation of the mode, not a bug to chase.

---

# AMENDMENTS 6 — port

**The port is 21118**, not 7420. Owner's assignment. Every default, doc, test,
example, ingress config and dev proxy uses 21118. `references/` is historical and
is left alone.

---

# AMENDMENTS 7 — speed, and no-auth on localhost

## F1. Localhost needs no token — but Origin/Host pinning is NOT optional

Owner's call: in `local` mode (bound to 127.0.0.1) no device token is required.
Correct — anyone who can reach loopback already has shell on the box, so a token
adds nothing.

**But do not read "no auth" as "no checks."** The real attack on a loopback
service is not a local user, it is the BROWSER: any website you visit can issue
requests to 127.0.0.1, and DNS rebinding turns a hostile page into a client of
this server. That is an actively exploited class of bug against local dev
servers, and this server runs arbitrary commands.

So in local mode, these are mandatory and replace the token:
- **Origin allowlist, strictly enforced on the WS upgrade and every non-GET.**
  Accept only `http://127.0.0.1:<port>` and `http://localhost:<port>`. A missing
  Origin on a browser-initiated WS upgrade is REJECTED, not defaulted-allow.
- **Host header pinning.** Reject any request whose Host is not `127.0.0.1:<port>`
  or `localhost:<port>`. This is what defeats DNS rebinding: the rebound name
  arrives in Host and will not match.
- Keep `SameSite` and never reflect arbitrary CORS origins.

Mode summary:

| mode       | bind      | device token | Origin + Host pinning |
|------------|-----------|--------------|-----------------------|
| local      | 127.0.0.1 | NOT required | REQUIRED              |
| lan        | 0.0.0.0   | REQUIRED     | REQUIRED              |
| cloudflare | 127.0.0.1 | REQUIRED     | REQUIRED              |

Auth is skipped ONLY because the bind address is loopback — never because a
request merely claims to be local. Derive it from the listener, never from a
header, and never from `X-Forwarded-For`.

## F2. Speed: the renderer is not the bottleneck; latency is

Owner wants it extremely fast. Spend the effort where it is felt:

**1. Predictive echo is PROMOTED from phase 2 to v1 (was B10).** Paint time is
~1ms; tunnel RTT is 30-150ms. Local echo is the only thing that removes that from
perception. Mosh-style: predictions start TENTATIVE and INVISIBLE, go CONFIDENT
and render only after one is confirmed by real server output, and any mismatch
wipes all pending predictions back to tentative — so a phantom character is never
shown. This is the single largest perceived-speed win available and nothing else
comes close.

**2. Renderer order**: WebGL where it is proven, Canvas where WebGL is risky, DOM
only as a last resort. Keep the probe-and-degrade machinery from B8 exactly as
built — do NOT force WebGL on mobile to chase paint throughput. Mobile WebGL
context loss renders a blank black rectangle, and canvas already paints far
faster than the network delivers. Trading a correctness cliff for an
imperceptible gain is a bad trade.

**3. xterm settings that actually cost frames:**
- `cursorBlink: false` — blinking forces a repaint cycle ~2x/sec forever, on an
  otherwise idle terminal. Biggest free win.
- `smoothScrollDuration: 0`.
- Never set `minimumContrastRatio` — it forces a per-cell colour computation on
  every paint (we already omit it for fidelity; it is also expensive).
- Load the terminal webfont and await `document.fonts.ready` BEFORE constructing
  the terminal — otherwise cell metrics are measured against a fallback font and
  the whole grid reflows on font swap.
- Feed `Uint8Array` to `write()`, never a string (no UTF-8 round trip).

**4. Already in and keep**: bytes bypass React entirely, same-tick coalescing,
deflate threshold 1024 so keystrokes skip compression, wheel-drop under
backpressure but never keystrokes, pooled zero-alloc buffers on the fanout path.

**5. Measure, do not assert.** The harness must report keystroke->echo latency at
p50/p95 in all three modes. A claimed number is not a number.

---

# AMENDMENTS 8 — all plugin entrypoint ids are namespaced `hex:`

`herdr plugin action invoke <ACTION_ID>` takes `--plugin` as OPTIONAL, so a bare
`open` is ambiguous the moment another installed plugin also defines `open`.
Every action, pane and link-handler id is therefore prefixed `hex:`
(hex = herdr-expose):

  hex:open · hex:pair · hex:status · hex:expose · hex:unexpose
  hex:pair-qr (pane) · hex:status (pane) · hex:expose-url (link handler)

**Verified against live Herdr 0.9.0**, do not re-derive:
- `:` in an id is ACCEPTED. Linked, listed, invoked, and the command log shows
  `succeeded` with real stdout.
- **`.` in an id is REJECTED** (`invalid_plugin_action_id`) — so `hex.open` does
  not work. `-` and `_` are also accepted.
- A `[[link_handlers]]` `action` field must reference the PREFIXED action id.

---

# AMENDMENTS 9 — `share`: one scoped, time-boxed, self-destructing tunnel

Owner's ask: from inside an agent session, say "expose this agent session on
myxxx.yy.com" and get back a URL — only that session, default 1 hour, gone after.

## G1. The command

```
herdr-expose share --domain agent1.deemwar.com [--session NAME] [--pane TARGET] [--hours 1]
```
Defaults: `--session` = the session the command runs in (`$HERDR_SESSION`, else
resolved from `$HERDR_SOCKET_PATH`), `--hours` = 1.
Prints: the URL, a pairing code, and the exact expiry time.

## G2. Scope is enforced in the SERVER, not the UI

New flag `--only <session>` / `--only-target <session>/<pane>`. The scope filter
applies in the store/view layer, so a scoped instance's tree CONTAINS only what
is shared. Everything else does not exist to that instance: not hidden in the
client, absent from the tree, absent from subscribe, and rejected at the hub if
a target outside scope is requested. A scoped share must never be one client
bug away from exposing the other sessions.

Command pass-through is also narrowed for a scoped instance: prompt / send-keys
/ read / scroll / resize on in-scope targets only. No `pane.run`, no session
enumeration, no plugin methods.

## G3. Ephemeral instance, not the main daemon

`share` spawns a SEPARATE detached herdr-expose on its own free port, with its
own state dir under `<state>/shares/<id>/`, its own auth store, and `--only`
set. The long-lived daemon on 21118 is untouched and keeps serving everything.
One share = one process = one port = one domain = one scope. Killing a share
cannot affect the main instance or another share.

## G4. Ephemeral DNS — this is the ONE exception to AMENDMENTS 3

C1 said tunnels are static and `stop` never removes DNS. That rule exists so a
PERMANENT endpoint keeps a stable hostname. A share is the opposite: it is
disposable by construction, so at expiry it MUST `destroy` — stop cloudflared,
delete the DNS record (only if tagged `herdr-expose-share`), delete the tunnel,
wipe the share state dir including its tokens. Leaving a dead hostname resolving
to a tunnel that no longer exists is worse than no record.

The domain's zone must be in the account the token can reach; fail early and
clearly if not (`herdr-expose share` checks zone access before creating anything).

## G5. Expiry is enforced three ways, because one is not enough

1. In-process timer: at TTL, the instance destroys its tunnel+DNS and exits.
2. Every issued device token carries `expires_at` <= the share's expiry, so a
   token cannot outlive the share even if teardown fails.
3. A `share.json` in the share dir records the deadline, and both
   `herdr-expose share list` and the MAIN daemon's periodic sweep reap shares
   whose deadline has passed and whose process is gone (crash-safe cleanup).

`share list` / `share extend <id> --hours N` / `share revoke <id>` complete it.
Revoke is immediate and idempotent.

## G6. Pairing still applies

A share is on the public internet, so it is NOT open. It mints a pairing code at
start, printed locally alongside the URL and the QR. Same rules as everywhere:
codes are displayed only on the machine, no endpoint mints one, tokens are
hashed at rest, and here they additionally expire with the share.

---

# AMENDMENTS 10 — every share is time-based. No exceptions.

Owner: "all are time based." There is NO permanent/unlimited share mode.
A long-lived share is a long TTL — `--days 30`, not a different code path.
`--hours 1` stays the default; `--days N` is sugar for hours.

Why this matters more than it looks: a single code path means there is no
branch where a share outlives its token, escapes the sweep, or skips teardown.
G5's three-way enforcement and G4's destroy-on-expiry apply universally.

**Restore across restarts.** A long share outlives a reboot, so the main daemon
restores running shares from `share.json` at startup — respawned with their
ORIGINAL deadline, never a refreshed one. A share whose deadline already passed
is reaped instead (tunnel + DNS + state destroyed), so a dead process can never
leave a live hostname resolving to nothing, and can never come back with a fresh
clock.

**Extending** is an explicit, logged, revocable act: `share extend <id>
--hours N | --days N` pushes the deadline on `share.json` AND on the
already-issued device tokens, or the tokens expire underneath a live share.

---

# AMENDMENTS 11 — the config documents itself

Owner: "create the config automatically with empty so they know — need otel,
logs and all stuff."

## H1. First run writes the COMPLETE config, not a minimal one

Every section and every key is present with its default value and a one-line
comment saying what it does. A key that exists but is not written is a key
nobody will ever find. Optional subsystems (otel, logging to a file, share
defaults, limits) appear too, switched off, so their existence is discoverable
without reading the source or the docs.

Rules that still hold: unknown keys are preserved on rewrite, missing keys take
defaults, and an older config loads on a newer binary. Adding a section must
never rewrite or reorder what the user has already edited.

`herdr-expose config print-default` prints the same scaffold to stdout (mirrors
`herdr --default-config`). `config path` / `config show` / `config edit`.

## H2. Sections

```toml
[server]
port = 21118
# No `bind` key. The resolved exposure mode decides the address — see [expose].

[auth]
pairing_ttl_seconds = 600
max_devices = 32
device_ttl_days = 30
pair_attempts_per_minute = 10
handshake_attempts_per_minute = 60

[ui]
theme = "auto"              # auto | light | dark
default_view = "grid"       # grid | focus

[expose]
cloudflare = false
domain = ""                 # REQUIRED when cloudflare = true
tunnel_name = "herdr-expose"
ngrok = false               # REMOVED by AMENDMENTS 18: no longer written, still loads if present
lan = false                 # bind 0.0.0.0; also the automatic fallback
autostart = false

[share]
domain_suffix = ""          # `share --name review` -> review.<suffix>
default_hours = 1
default_mode = "auto"       # auto | lan | domain
max_concurrent = 10

[log]
level = "info"              # debug | info | warn | error
format = "text"             # text | json
file = ""                   # empty = stderr, captured by the service manager

[otel]
enabled = false
endpoint = ""               # e.g. http://10.8.0.4:4318
protocol = "http/protobuf"  # http/protobuf | grpc
service_name = "herdr-expose"
traces = true
metrics = true
logs = false
```

## H3. OTEL credentials are env-only. No exception.

OTLP headers routinely carry an auth token. **There is no header/token key in
the config file and there never will be.** Headers come from
`$OTEL_EXPORTER_OTLP_HEADERS`, read by name at point of use, and the value must
never reach a log line, `status` output, an error message or a state file — the
same rule as the Cloudflare token. A config file is something people paste into
issues; a token must not be sitting in it.

## H4. What is worth exporting

Spans on the paths we already budget: upstream connect/resync, terminal stream
spawn, share create/teardown, tunnel provisioning. Metrics: the existing
`/v1/metrics` latency histograms, frames/bytes per second, connected clients,
connected sessions, active shares. `logs = false` by default — terminal output
must NEVER be exported; it is the user's screen contents, which can contain
anything, and shipping it to a collector is a data-exfiltration path, not
telemetry.

---

# AMENDMENTS 12 — no OTEL. Local log only. (WITHDRAWS H3/H4)

Owner: "no otel machine, just local log please." AMENDMENTS 11 §H3 and §H4 are
withdrawn in full: no `[otel]` section, no OTLP exporter, no
`go.opentelemetry.io` dependency. The rest of AMENDMENTS 11 stands.

```toml
[log]
level       = "info"   # debug | info | warn | error
format      = "text"   # text | json
file        = ""       # empty = <state>/herdr-expose.log
max_size_mb = 10       # rotate past this
keep        = 3        # rotated files retained
```

- **Default to a FILE, not stderr.** A detached daemon's stderr goes nowhere,
  and "no way to see what the daemon did" is the problem being solved.
- **Rotate in-process, no dependency.** This runs for weeks; an unbounded log is
  a disk-filling bug.
- **Shares log to their own file** in the share's state dir, so a share's life is
  auditable on its own and the log dies with the share.
- **Never log pane bytes or secrets, at any level including debug.** Terminal
  output is the user's screen and can contain anything; tokens are logged as ids
  or hashes, never values. Enforced in code, following the existing
  `internal/expose` redactor.
- Log what answers "what happened": start/stop with mode+bind, session
  connect/disconnect, tunnel provisioning and teardown, share
  create/extend/revoke/expire, pairing issued/redeemed/failed with device id,
  auth rejections with reason, upstream reconnects.

`herdr-expose logs [--follow] [-n N] [--share <id>] [--json]`.
`doctor` reports the log path, size and writability.

---

# AMENDMENTS 13 — agent panes get a TRANSCRIPT view, not a terminal mirror

Owner, looking at a real Claude pane in the browser: "not proper, often getting
nonsense, need some better representation."

## J1. The diagnosis

Mirroring an agent's TUI grid is the wrong abstraction and cannot be fixed by
making the mirror better:

- Claude Code (and Codex, and most agent TUIs) paint incrementally near the
  BOTTOM of the grid and repaint on SIGWINCH. Attaching a browser resizes the
  PTY, so the agent throws away its screen and redraws — you see a fragment in a
  void, and the history you attached to read is gone.
- The PTY grid is a fixed cols x rows. A phone is not. Reflowing is impossible
  because the server only has the post-layout character grid.
- What the user wants from a phone is the CONVERSATION and the STATE — what did
  I ask, what did it say, is it stuck, what do I answer. Not a faithful
  reproduction of a 100x30 character matrix.

## J2. Two views, chosen by what the pane is

- **Transcript view — the DEFAULT for a pane with an agent.** Reflowed, readable
  text: the recent conversation, ANSI stripped, wrapped to the VIEWPORT width,
  not the PTY width. Plus the agent's state, a prompt box that sends
  `agent.prompt`, and the key bar (y/n/enter/esc/arrows/1/2/3) when blocked.
- **Terminal view — the default for a pane with NO agent**, and available on any
  pane via a toggle in the header. This is today's xterm mirror, unchanged. It
  is the right tool for a shell, for a TUI, and for when you need exactness.

The toggle is remembered per pane.

## J3. Transcript view MUST NOT resize the PTY

This is the point. A transcript subscriber declares no geometry, so the agent's
own screen is never disturbed — no SIGWINCH, no redraw, no lost history. Opening
a pane on a phone becomes non-destructive, which it is not today.

Server: a new viewport mode `transcript`, polled at ~1Hz via `agent.read` /
`pane.read` (`--source detection` when blocked, otherwise recent lines), ANSI
stripped SERVER-side, delivered as a control-plane frame — not binary, not
xterm. Geometry is never sent for a transcript target.

## J4. Honesty about what a transcript is

It is a rendering of the agent's visible buffer, not a true conversation log —
Herdr exposes the screen, not the agent's message history. So: do not fake
structure we cannot know. Show the text the agent is displaying, cleanly
reflowed, with clear separation between the agent's output and our chrome. If
the buffer is all we have, say what it is rather than implying a transcript we
did not actually reconstruct.

---

# AMENDMENTS 14 — LOOKING MUST NOT TOUCH (supersedes B2 and J2's defaults)

Owner, working at his laptop with the web UI open on the same session:
**"you are scrolling actual herdr terminal."** Opening a pane in the browser
moved the pane he was typing in — reflow, redraw, scroll, under his hands.

## K0. What was actually measured (herdr 0.9.0, throwaway session, `tput` inside the pane as ground truth)

| call | pane before | during | after |
|---|---|---|---|
| `terminal session observe <p>` | 120x40 | 120x40 | 120x40 |
| `terminal session observe <p> --cols 100 --rows 60` | 120x40 | 120x40 | 120x40 |
| `terminal session control <p> --cols 100 --rows 60 --takeover` | 120x40 | **100x60** | **100x60** |
| `terminal session control <p> --takeover` (no geometry) | 41x13 | **120x40** | **120x40** |
| `pane.read` (visible / recent_unwrapped / detection), `agent.read`, `session.snapshot` | — | unchanged | unchanged |
| `pane.scroll {offset_from_bottom:20}` | offset 0 | **offset 20** | **offset 20** |

`scroll.viewport_rows` tracks the PTY (40 -> 60 with the control resize). Nothing
in the 0.9.0 API reports a pane's **cols**; the tab layout's `rect.width` is the
only width anywhere, and an observe attached with **no** `--cols/--rows` reports
the pane's real size as the first frame's `width`/`height`.

So the mutating paths were: the CONTROL upgrade (which any keystroke triggers)
and `pane.scroll`. Observing was already harmless — the geometry we sent it was
simply ignored.

## K1. The law

**Viewing a pane from the web must never mutate the owner's local session.**
Only an explicit, informed action may.

## K2. Transcript is the default for EVERY pane (supersedes J2)

Agent or plain shell. A shell is output like any other output and renders fine
as text; "no agent" was never a reason to attach to somebody's terminal. The
`text`/`term` toggle stays and is still remembered per pane.

## K3. Terminal view is opt-in, once per pane per session

The first tap on `term` states in one line what it costs and waits. Consent
lives in `sessionStorage` (`hex.termok.<target>`) — that is exactly the lifetime
of "do not nag again for the session". Declining does not even record the
preference.

## K4. Match the pane; never impose on it (WITHDRAWS B2)

B2 said "a terminal without a geometry message does not work" and required
`resize` before the first frame. That is withdrawn — it is what made merely
opening a pane declare a size for it.

- A LIVE attach passes **no** `--cols/--rows`. Herdr uses the pane's own size and
  reports it; the server relays it as a new `geometry` control frame
  (`{target, cols, rows, source:"pane"|"client"}`).
- The client renders **at** that grid and scales the FONT to fit its box. It
  never resizes the pane to fit the browser.
- The observe->control upgrade reuses that same reported size, so taking control
  is control only.
- `resize` survives as the **explicit** action ("fit to my window"), is reversible
  with `{"match":true}`, and is the only thing in the product that changes a
  pane for everyone attached to it.

## K5. Read paths are read-only, and `seen` stays ours

Summary polls, transcript polls, detection reads, agent reads and snapshot
requests are all `pane.read` / `agent.read` / `session.snapshot` — measured
non-mutating. Nothing calls `pane.focus` or `agent.focus`, and nothing may:
herdr's focus marks panes seen and would wipe the owner's own Done badges.
`seen` stays per-connection in `core.SeenSet` and never leaves this process.

`pane.scroll` is removed from the observer path entirely. A viewer scrolls their
own 5000 lines of scrollback; only a controller moves the shared viewport.

## K6. The tree is refreshed while somebody is looking (found during the audit)

`events.subscribe [{"type":"pane.agent_status_changed"}]` is refused on 0.9.0
with `missing field pane_id`, so there is no session-wide push for the one field
the whole UI is about. An agent that finished kept its `working` badge until
some unrelated structural event happened to fire a resync. The hub now re-reads
the tree every 1.5s **while at least one client is connected**, and not at all
otherwise — `session.snapshot` is read-only, and an empty room generates no
upstream traffic.

---

# AMENDMENTS 15 — quick tunnels come back, for SHARES only

Owner: "sometimes just cloudflare is enough and sometimes domain."

AMENDMENTS 3 (C1) deleted the quick-tunnel path outright. That was right for the
PERMANENT deployment and wrong as a blanket rule. The objections — a hostname
that changes on restart breaks PWA install, bookmarks and origin-bound device
tokens — are all about an endpoint you return to daily. A share is disposable by
construction, so none of them bind.

Cloudflare's own description of TryCloudflare is the share contract almost word
for word: no account, no DNS, no open ports, ~3s setup, automatic HTTPS and edge
DDoS mitigation, "ephemeral by design — the tunnel dies with the process,
nothing to revoke, nothing to clean up."

## K1. Three share transports, one ladder

```
herdr-expose share --lan                 http://<lan-ip>:<port>     no internet
herdr-expose share --quick               https://<random>.trycloudflare.com
herdr-expose share --domain x.you.com    https://x.you.com          stable, your zone
```

Auto (no flag) resolves: `--domain` if `[share].domain_suffix` or `[expose].domain`
gives a usable hostname AND cloudflared resolves -> else `--quick` if cloudflared
resolves -> else `--lan`. Print one line naming the choice and the reason.

**`--quick` should be the default for a share on a machine with no configured
domain**, because it is the only transport that works with zero setup, zero
account and zero cleanup. It also removes today's hard limitation that a share
requires a zone in the owner's Cloudflare account — with `--quick`, anyone can
run this.

## K2. What stays true for a quick share

Scope enforced server-side, pairing required, time-boxed with the same three-way
expiry, `share list/extend/pair/revoke/restore` all identical. A quick tunnel is
on the public internet, so it is NOT more trusted than a domain share.

Teardown is simpler, not weaker: no DNS record and no named tunnel exist, so
expiry stops the process and wipes the state. Nothing to delete in Cloudflare,
and `revoke --all` must handle lan + quick + domain shares in one pass.

## K3. Say the trade-off out loud

A quick share's hostname is new every time, so **device tokens do not carry
over** — they are origin-bound. Each quick share needs a fresh pairing scan.
That is fine for a throwaway and unacceptable for the daily driver, which is
exactly why `[expose]` keeps its static domain and `--quick` is confined to
shares. The CLI should say this once, on create, rather than letting the user
discover it by re-pairing.

Permanent deployment: unchanged. `[expose] cloudflare = true` still REQUIRES
`domain`, still refuses to be ephemeral. C1 stands where it was aimed.

---

# AMENDMENTS 16 — least exposure by default. The ladder is climbed, never guessed.

Owner: "default is local, and then they ask for lan, and they ask for
cloudflare, and cloudflare custom domain."

This WITHDRAWS the auto-escalation in K1. A bare `share` currently resolves
domain -> quick -> lan, which means the tool can put a session on the public
internet because a config key happened to be set. Exposure must always be a
thing the user asked for.

## L1. Four rungs, each one an explicit request

| rung | flag | reach |
|---|---|---|
| **local** (DEFAULT) | none | `http://127.0.0.1:<port>` — this machine only |
| lan | `--lan` | `http://<lan-ip>:<port>` — anyone on the wifi |
| cloudflare | `--quick` | `https://<random>.trycloudflare.com` — the internet |
| custom domain | `--domain X` | `https://X` — the internet, on a name you own |

No flag means **local**. Never quick, never lan, never a configured domain.
The same ladder governs the daemon: `[expose]` with nothing set is local, and
`cloudflare`/`lan` are opt-in keys, which is already true — do not change it.

## L2. A failed request degrades LOUDLY; it never quietly climbs

Fallback stays, but only as the failure mode of an EXPLICIT ask, never as a
default. `--quick` or `--domain` with no cloudflared resolvable falls back to
lan with a printed reason, because the user did ask to be reachable and a LAN
address is the nearest honest answer. But nothing ever escalates ABOVE what was
requested: a `--lan` request never becomes a tunnel, and a bare `share` never
becomes anything but loopback.

`[share] default_mode` still exists for someone who wants a different personal
default, but its shipped value is `local`.

## L3. Why this is worth the small inconvenience

This binary runs arbitrary commands in the user's agent sessions. The blast
radius of each rung is different by orders of magnitude — this machine, this
room, the entire internet — and the difference between rung 1 and rung 3 must
never be a config file the user forgot they set. Typing `--quick` takes a
second and makes the reach a conscious choice, which is the only way the
security properties elsewhere in this spec (pairing, scope, TTL) mean anything.

---

# AMENDMENTS 17 — a SHARE defaults to lan; the DAEMON defaults to local

Owner: "i think no flag means lan and local, no one wants to do only localhost
right." Correct, and it corrects L1.

A share exists to be reached from somewhere else. Someone sitting at the machine
would open the main daemon on `localhost:21118`; a loopback-only share is the one
rung that makes the feature pointless.

| rung | flag | reach |
|---|---|---|
| **lan (DEFAULT for a share)** | none | `http://<lan-ip>:<port>` — this machine AND this network (0.0.0.0 covers both) |
| cloudflare | `--quick` | `https://<random>.trycloudflare.com` |
| custom domain | `--domain X` | `https://X` |
| local | `--local` | `http://127.0.0.1:<port>` — explicit opt-in, for testing |

`[share] default_mode` ships as `lan`.

**The daemon's own `[expose]` default stays `local`.** Same principle — least
exposure that still does the job — different job: the daemon serves the person
at the machine, so loopback is right there; a share's whole reason to exist is
reach, so lan is right here.

L2 is unchanged and remains the important half: **the ladder is never climbed
for you.** A bare `share` must never become a tunnel because a domain happens to
be configured. Fallback degrades downward on an explicit ask, never upward.

---

# AMENDMENTS 18 — one built-in transport. Everything else is an adapter.

Owner: "no need ngrok — cloudflare is enough."

## M1. The built-in ngrok provider is REMOVED

Gone: the `ngrok` config key, `--provider`, the ngrok rungs, `$NGROK_AUTHTOKEN`
handling, and every claim in the README, the skill, the docs and the config
scaffold that this binary carries ngrok. AMENDMENTS 3 §C4's ngrok clause is
withdrawn; its static-domain rule stands for Cloudflare.

## M2. The reason is UNVERIFIED SURFACE, not an unwanted feature

There is no ngrok binary and no ngrok token on the machine this was built on, so
the provider was unit-tested against a fake binary and **never once exercised
end to end**. Those tests proved the shape of the integration — argv, the token
in the child's environment and nowhere else, the scanner regex — and could not
prove ngrok's agent behaves that way.

**Code on a public surface that has never actually run is a liability.** It rots
against the upstream tool's flags and log format, and the first person to use it
finds the bug. This is the criterion for admitting a future built-in: not "is it
popular" but "can we run it end to end here".

## M3. What is KEPT, and why it is not ngrok scaffolding

- **The `Provider` interface and `Footprint()`.** `Footprint` is what makes
  teardown symmetric with creation *when creation was interrupted* — the case
  that orphans resources — and the interface is what lets idempotency be tested
  without a network. cloudflare-named, cloudflare-quick and the goja adapter all
  implement it, the rung × provider table still runs over all three, and the
  abstraction does not collapse back into Cloudflare-specific code.
- **The goja/JS adapter**, promoted from curiosity to THE extension point. It
  is now the answer for ngrok, tailscale, a corporate proxy or a homelab, so it
  is documented as such (`adapters/template.js`) and held to the built-in's
  standard in the provider table rather than tested beside it.
  `adapters/ngrok.js` stays as a worked example, clearly labelled as
  community-shaped code that CI parses and never runs.

## M4. `--provider` is removed, not reduced

With one built-in it would be a one-value flag pretending to be a choice. It is
refused at parse time with an error naming the replacement, because silently
accepting `--provider ngrok` and handing back a Cloudflare tunnel is the one
outcome worse than failing.

## M5. An old config still loads

A file carrying `ngrok = true` (under `[expose]` or `[share]`) is read without
complaint, ignored, and dropped on the next rewrite — exactly as the withdrawn
`server.bind` and `default_mode = "auto"` keys are. A key that no longer does
anything is not a reason to take somebody's daemon down.

The four rungs of AMENDMENTS 16/17 are unchanged: bare = lan, `--quick`,
`--domain`, `--local`, and the ladder is still never climbed for you.

---

# AMENDMENTS 19 — what a slow client ACTUALLY gets (corrects B9's backpressure clause)

B9 said: "if the socket's buffered amount exceeds ~256KB, drop WHEEL/scroll input
but never keystrokes." That describes a degradation ladder that ends in a `gap`.
Half of it is true; the ending is not, and the spec has been claiming a mechanism
that does not exist in the code.

## N19.1 What the stress test measured

A client that accepts a connection and then never drains it, against a busy pane:

| observation                | measured                                    |
|----------------------------|---------------------------------------------|
| time to disconnect         | ~20s                                         |
| degradation phase before it| none observed                                |
| `gap` frame to that client | none                                         |
| server RSS over 180s       | +0.2MB — bounded                             |

The **safety** property B9 exists for holds: one stalled viewer cannot grow the
server's memory, and cannot stall the hub for anybody else. The described
*mechanism* is what is wrong.

## N19.2 What the code does, in three layers

1. **Inbound scroll shedding — REAL, and exactly as B9 says.** `backlogged()` is
   `queued > SocketBacklogBytes` (256KB), and a `scroll` message from a backlogged
   connection is dropped on the floor. Keystrokes are never shed. This is the half
   of B9 that shipped (`internal/serve/ws.go`).
2. **Hard disconnect on a full outbound queue.** The per-connection queue is
   `SendQueueDepth` = 512 messages. `SendBinary`/`SendJSON` never block: on a full
   queue they release the buffer, log `ws: send queue full, closing connection`,
   and `shutdown()` the socket. There is no throttle, no partial send, no
   half-speed mode between 256KB and closed — 256KB of backlog buys the client
   only a scroll-free ride toward the same door.
3. **`gap` is an UPSTREAM-side frame, not a backpressure signal.** Type-3 `gap`
   (`internal/core/wire.go`) is emitted by the coalescer when a *target's* buffer
   overflows or its upstream stream restarts, and it is followed by a repaint.
   It reports bytes lost between Herdr and the hub. It is never the answer to a
   client that is not reading its socket, because a client that is not reading its
   socket cannot receive it.

## N19.3 Which behaviour is INTENDED

**The code's.** Disconnect is the right policy here and the spec moves to it:

- A `gap` to a stalled client is undeliverable by construction. Queuing one
  behind 512 messages the client is already not reading is theatre.
- A degradation ladder nobody can observe is a ladder nobody can test. This one
  was never observed in 180s of stress because it is not there.
- The viewer is a web app with reconnect. Closing is a two-second recovery for a
  client that is genuinely wedged, and it releases pooled buffers immediately —
  which is *why* RSS stayed flat at +0.2MB.
- Silence has a bound: 512 messages or 256KB of backlog, whichever comes first.
  An unbounded queue to a dead reader is the classic way to turn one bad client
  into an OOM.

**Recommendation on record: the SPEC moves, the code stays.** The one thing worth
adding later is honesty on the way out — a close code and reason (`1008`,
"send queue full: client could not keep up") so the client can tell "you were too
slow" from "the network dropped" and say so in its reconnect banner. That is a
UX improvement, not a change of policy, and it is not required for v1.

## N19.4 The corrected clause

> Backpressure: a connection whose queued bytes exceed `SocketBacklogBytes`
> (256KB) has WHEEL/scroll input shed; keystrokes are never shed. A connection
> whose outbound queue reaches `SendQueueDepth` (512 messages) is CLOSED — the
> hub never blocks on a client and never grows a queue to fit one. There is no
> intermediate degradation phase and no `gap` on this path; `gap` reports bytes
> lost upstream of the hub, and a repaint follows it.

---

# AMENDMENTS 20 — the memory budget measures the whole footprint, or it measures nothing

A3's memory row reads "RSS, 20 panes < 60MB". It was measured as the server
process alone, and it is wrong in two separate ways: the number is not achievable,
and the thing it counts is not the thing the user pays for.

## N20.1 What was measured

| observation                                    | measured        |
|------------------------------------------------|-----------------|
| server process RSS, loaded, plateau             | ~88MB           |
| per LIVE target, `herdr terminal session observe` subprocess | ~8.7MB **per connection** |
| 1 pane, 16 viewers, total                       | ~223MB          |

## N20.2 Why counting only the server is the wrong unit

B1 (AMENDMENTS 2) replaced "one upstream stream per target" with **per-connection
streams**, and A3 was never reconciled with it. A3 still carries the old
invariant in its prose ("one upstream stream per target no matter how many
clients") beneath a table that counts panes, not viewers. Under B1 the dominant
term is not panes at all — it is `observe` subprocesses, one per LIVE target per
connection, and each is a process this binary spawns and is responsible for.

A budget that excludes the subprocesses the design mandates is not measuring the
product's footprint; it is measuring one process inside it. `ps` on the machine
tells the user the truth either way, so the budget may as well.

## N20.3 The corrected budget

The unit is **RSS of the herdr-expose process + the RSS of every
`herdr terminal session observe` child it has spawned**, sampled at plateau
(after the tree is loaded and every viewer has attached), not at startup.

| path                                         | target                         |
|----------------------------------------------|--------------------------------|
| server process RSS, 20 panes, no LIVE viewer | < 100MB (measured plateau ~88MB) |
| per LIVE target-connection (observe child)   | < 10MB (measured ~8.7MB)        |
| TOTAL, 20 panes + 16 LIVE viewers of one pane| < 250MB (measured ~223MB)       |
| idle CPU, 20 panes                           | < 1%                            |

60MB is retired. It was never met and pretending otherwise made every other
number in the table less believable. ~88MB is a Go server with an embedded web
bundle, a goja runtime and pooled fanout buffers; it is a fair price and it is
now the stated one.

## N20.4 The per-connection term is a BUG against this budget, not a licence

The `< 10MB` row prices what the code does today; it does not bless it. Under
B1 the `observe` child is per connection, so 16 viewers of ONE pane spawn 16
children of the same pane — the linear term that turns 88MB into 223MB. The
fan-out fix (one upstream stream per target, shared by every connection, which
is what A3's prose asked for all along) is in progress. When it lands, the TOTAL
row drops to `< 120MB` for the same 20-pane / 16-viewer shape and the
per-connection row becomes `< 1MB`. **Amend this table when that is MEASURED,
not when it is merged.**

## N20.5 The rule this came from

A budget that is quietly missed is not a budget; it is a wish with a table
around it. Every row here is a number somebody watched a process reach. A3's
closing line still governs: *a claimed number is not a number*.
