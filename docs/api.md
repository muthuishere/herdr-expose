# herdr-expose API v2

This document is the **public contract** for herdr-expose. It is written so that
a Swift, Kotlin, Rust or Python client can be built from this page alone, with
no access to the server's source code. If something a client needs is not here,
that is a bug in this document — please report it.

The web UI shipped inside the binary is simply the first client of this API. It
has no privileged path, no private endpoints, and no frame types that are not
documented below.

- **Base URL**: `http://127.0.0.1:21118` by default (port is configurable).
- **Transport**: HTTP/1.1 and a single WebSocket.
- **Version**: wire `api: "2"`. The HTTP paths stay `/v1/...`; the negotiated
  version is the `api` field in `/v1/config` and `welcome`.
  See [Versioning](#11-versioning-and-compatibility).
- Verified against Herdr 0.9.0, protocol 22.

## What changed in API 2 — MULTI-SESSION

The owner runs many named Herdr sessions at once and wants all of them in one
UI, so the server is no longer bound to a single socket. Two breaking changes:

1. **The tree gained a top level.** It is now
   `sessions[] -> workspaces[] -> tabs[] -> panes[]`. A session is
   `{id, name, running, connected, focused, origin, workspaces[]}`, where `id`
   is the SESSION NAME — stable across server restarts, unlike pane ids.
2. **Every `target` is session-qualified**: `herdr-plugins/w2:p1`, never a bare
   `w2:p1`. Each Herdr session mints its own ids starting at `w1:p1`, so bare
   ids collide across sessions and would route a keystroke to the wrong
   machine's pane. The qualified form is used everywhere a target appears:
   `subscribe`, `viewport`, `resize`, `scroll`, `seen`, the binary frame header
   (both directions) and `closed`/`agent`/`error` frames. Workspace and tab ids
   are qualified the same way.

The session the server was launched from (`$HERDR_SOCKET_PATH`) is one entry
among many, flagged `origin: true`. It gets no other privileges. Sessions appear
and disappear while the server runs — a session going down marks its entry
`connected: false` and leaves every other session streaming.

---

## Contents

1. [Threat model — read this first](#1-threat-model--read-this-first)
2. [HTTP endpoints](#2-http-endpoints)
3. [Authentication and pairing](#3-authentication-and-pairing)
4. [The WebSocket: two planes](#4-the-websocket-two-planes)
5. [Control plane — JSON frames](#5-control-plane--json-frames)
6. [Data plane — binary frames](#6-data-plane--binary-frames)
7. [Sequence numbers, gaps and reconnection](#7-sequence-numbers-gaps-and-reconnection)
8. [Viewport modes and geometry](#8-viewport-modes-and-geometry)
9. [Calling Herdr methods](#9-calling-herdr-methods)
10. [Errors](#10-errors)
11. [Versioning and compatibility](#11-versioning-and-compatibility)
12. [A minimal client, end to end](#12-a-minimal-client-end-to-end)

---

## 1. Threat model — read this first

This API can run arbitrary commands on the machine hosting it. `pane.run` and
`agent.prompt` are, by design, remote code execution as the logged-in user.

The server runs in one of **four exposure modes**, and the mode decides both the
bind address and whether you need a token.

**The mode is always something a human asked for** (AMENDMENTS 16). The ladder —
`local` -> `lan` -> `quick` -> `cloudflare` — is climbed only by an explicit
flag or config key, never automatically: the daemon defaults to `local`, a
`share` defaults to `lan`, and nothing escalates above what was requested. A
failed explicit request for a tunnel may degrade DOWN to `lan`, with the reason
in `fell_back`. As a client, read `mode` from `GET /v1/config`; never infer
reach from the URL you were handed. **`secure_context` is NOT a field on
`/v1/config`** — it is reported by `herdr-expose status`, `herdr-expose expose
status --json` and `herdr-expose share list --json`, and in a browser you simply
read `window.isSecureContext`.

| mode | bind | device token | Origin + Host pinning | secure context |
|---|---|---|---|---|
| `local` | `127.0.0.1` | **not** required | required | yes (`localhost` is exempt) |
| `lan` | `0.0.0.0` | **required** | required | **no** |
| `quick` | `127.0.0.1` + tunnel | **required** | required | yes |
| `cloudflare` | `127.0.0.1` + tunnel | **required** | required | yes |

- **`local`** — the **daemon's** default, and `herdr-expose share --local`.
  Anyone who can reach loopback already has shell on the box, so no token is
  required. Origin and Host pinning replace it, and they
  are strictly enforced: only `http://127.0.0.1:<port>` and
  `http://localhost:<port>` are accepted, a **missing `Origin` on a WebSocket
  upgrade is rejected**, and a `Host` that does not match is rejected. That is
  what defeats DNS rebinding, which is the real attack on a loopback service.
- **`lan`** — bound to `0.0.0.0`, reachable by anyone on the wifi, and the
  **default for `herdr-expose share`** (AMENDMENTS 16). A device token is
  mandatory. **Plain HTTP on a LAN IP is not a secure context**, so
  `secure_context` is `false` in `status` / `share list --json`, **the service
  worker does not register and
  the PWA cannot be installed**. Your client must detect this and not offer an
  install prompt that cannot work. A native mobile client is unaffected.
- **`quick`** — loopback plus an ephemeral TryCloudflare tunnel on a random
  `*.trycloudflare.com` hostname. https, so a secure context, and a device token
  is mandatory exactly as everywhere else: **an unguessable hostname is not a
  credential.** It exists only for `herdr-expose share`, never for the permanent
  deployment. The hostname is **new on every share**, and device tokens are
  origin-bound, so a client that stored one for a previous quick URL must pair
  again rather than retry — treat the `401` as "return to pairing". Do not offer
  a persistent "Add to Home Screen" for a `quick` URL: the install would be
  pinned to a hostname that stops existing when the share expires.
- **`cloudflare`** — loopback plus a static, API-provisioned tunnel on the user's
  own domain. The permanent deployment is ALWAYS this mode and never ephemeral:
  a hostname that changes on restart breaks PWA installs, bookmarks and
  origin-bound device tokens, which is precisely why `quick` is confined to
  time-boxed shares.

Read `mode` from `/v1/config`; never infer it from the URL. For secure-context,
use `window.isSecureContext` (browser) or the `secure_context` field of
`herdr-expose status` / `share list --json` (tooling).

A client author's obligations:

- Store the device token in the platform secure store — **Keychain** on
  iOS/macOS, **Keystore / EncryptedSharedPreferences** on Android. Never in plain
  preferences, never in a log line, never in a crash report.
- Use TLS for anything that is not literally `127.0.0.1`.
- Treat a `401` mid-session as "wipe the stored token and return to pairing", not
  "retry".

---

## 2. HTTP endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | none (detail requires it) | Liveness |
| `GET` | `/v1/config` | **none** | Client bootstrap. It is how you discover whether auth is required, so it can never require it. |
| `POST` | `/v1/pair` | pairing code | Exchange a pairing code for a device token |
| `GET` | `/v1/metrics` | required | Latency histograms for the budgeted paths |
| `GET` | `/v1/stream` | required | WebSocket upgrade |
| `GET` | `/*` | none | The embedded web app |

### `GET /healthz`

Liveness, and it answers **two different bodies** depending on who is asking.
That is deliberate: an anonymous prober behind a tunnel learns only that
something herdr-shaped is alive, while an authenticated client (or a loopback
caller under the local-mode bypass, which is what `status` and `doctor` use)
gets the operational detail.

```http
GET /healthz HTTP/1.1
```

Anonymous — exactly these two fields, and nothing else ever appears here:

```json
{ "ok": true, "api": "2" }
```

Authenticated (`Authorization: Bearer <token>`), or from loopback in `local`
mode:

```json
{
  "ok": true,
  "api": "2",
  "version": "0.1.0",
  "upstream": true,
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "sessions": 3,
  "sessions_connected": 2,
  "tree_rev": 41,
  "web_ui": true
}
```

Present a token only when you have one: an anonymous probe is never logged as a
rejection and consumes no rate-limit budget, whereas a probe carrying a dead
token is.

### `GET /v1/config`

Bootstrap for a client. **Unauthenticated, and never contains a token or any
secret.** Send your bearer token anyway when you have one — it is what makes
`authenticated` meaningful.

```json
{
  "api": "2",
  "version": "0.1.0",
  "stream": "/v1/stream",
  "mode": "local",
  "auth_required": false,
  "authenticated": true,
  "multi_session": true,
  "targets": {
    "format": "<session>/<pane_id>",
    "separator": "/",
    "default_session": "herdr-plugins"
  },
  "limits": { "min_cols": 20, "min_rows": 6 },
  "ui": { "theme": "auto", "default_view": "grid" },
  "scope": "",
  "exposure": { "url": "https://herdr.example.com", "healthy": true }
}
```

- `mode` is `local`, `lan`, `quick`, `cloudflare` or `js` (`js` is a JS
  adapter — the escape hatch for any transport that is not built in).
- **`auth_required` and `authenticated` are the two fields that matter most**,
  and they exist for one reason: a browser **cannot read the status of a failed
  WebSocket handshake**. The WebSocket API surfaces no close code when the
  upgrade is rejected with `401`, so a client that only retries the socket can
  never learn it needs to pair, and loops "reconnecting" forever. Read these
  two before you open the socket. `auth_required` is `false` only in `local`
  mode; `authenticated` reports whether the token you sent (if any) works.
- `scope` is present and non-empty on a **share-scoped instance**
  (`herdr-expose share`): it names the one session this instance can see. The
  rest of the machine is not hidden from you, it is absent.
- `exposure` is present only when a tunnel is up, and `exposure.url` is the
  public URL. In `quick` mode the edge assigns that hostname at start, so read
  it and do not remember it.
- There is **no `features` array, no `secure_context` and no `public_url`** in
  this body — earlier drafts of this document claimed all three. Secure-context
  is something you determine yourself (`window.isSecureContext`, or simply:
  plain HTTP on a LAN IP is not one); the public URL is `exposure.url`.

**Ignore fields you do not recognise.** New capabilities are announced by new
fields here rather than by bumping the API version.

### `GET /v1/metrics`

Authenticated. Returns the measured latency histograms for the paths the
performance budget names — decode-to-queued, queued-to-written — as
`{"<name>_us": {...}}` objects. It tells a caller how busy the machine is,
which is why it is not public. Its exact shape is diagnostic and may change;
do not build a client feature on it.

---

## 3. Authentication and pairing

There are **two kinds of secret**, with different jobs.

| | Server token | Device token |
|---|---|---|
| What it authorises | talking to this instance at all | one specific device |
| Where it comes from | generated at first run and printed **once**, on the daemon's first `serve` (it is stored only as a hash, so there is no way to show it again — `herdr-expose status` does NOT print it) | issued by pairing |
| Lifetime | until rotated | sliding 30 days from last use |
| Revocable individually | no | yes |

Both are presented the same way. The server stores **SHA-256 hashes only**, and
every comparison is constant-time.

### Presenting a token

```http
Authorization: Bearer <token>
```

For the WebSocket, where browsers cannot set headers, the query parameter is also
accepted:

```
GET /v1/stream?token=<token>
```

Prefer the header wherever your platform allows it — query strings end up in
proxy logs. Auth is checked **before** the WebSocket upgrade: a bad token gets an
HTTP `401`, not a socket that closes a moment later.

### Pairing flow

Pairing exists so a phone never has to type a long token. It is **not** needed in
`local` mode, where `auth_required` is `false`.

**The pairing code is only ever displayed on the physically present machine** —
in the terminal, or in the `hex:pair-qr` overlay pane inside the Herdr TUI. **No
HTTP endpoint mints or reveals a code.** `POST /v1/pair` only *accepts* one. This
is the whole reason `lan` mode is safe: somebody on your wifi can reach the port
and get precisely nowhere without looking at your screen.

```
  phone                         server                     user's laptop
    |                              |                             |
    |                              |<--- herdr-expose pair -------|
    |                              |  mints a 6-char code, 10 min |
    |                              |  prints it and a QR          |
    |                              |                             |
    |--- POST /v1/pair ----------->|                             |
    |    {"code":"7K2QX9", ... }   |  verify, single-use, burn it |
    |<-- 200 {"token": ...}        |                             |
    |                              |                             |
    |--- GET /v1/stream (Bearer) ->|                             |
```

The QR encodes the full URL so a scan needs no typing at all, and the host in it
follows the mode:

```
cloudflare   https://herdr.example.com/?pair=7K2QX9
quick        https://mid-ancient-stuff-tokyo.trycloudflare.com/?pair=7K2QX9
lan          http://192.168.1.24:21118/?pair=7K2QX9
local        http://127.0.0.1:21118/?pair=7K2QX9
```

In `lan` mode the IP is re-resolved on network change, so a DHCP lease change
does not leave a stale URL behind. In `quick` mode the hostname belongs to that
one share and is gone when it expires.

**`POST /v1/pair`**

Request — the server reads exactly these two fields. `name` is free text shown
in `herdr-expose devices` so a human can tell which device to revoke; anything
else you send is ignored.

```json
{
  "code": "7K2QX9",
  "name": "Pixel 8"
}
```

`200 OK`:

```json
{
  "token": "yq8Zr1Wd3pK7sVx0aB4cE6gH9jL2nP5tQ8uY1wA3zC7",
  "device": {
    "id": "dev_01HQ8R2K9M",
    "name": "Pixel 8",
    "created_at": "2026-09-16T09:12:00Z",
    "last_seen": "2026-09-16T09:12:00Z"
  },
  "expires_at": "2026-10-16T09:12:00Z",
  "expires_in": 2592000
}
```

The bearer token is `token`, at the top level. **`expires_at` and `expires_in`
always describe the same instant** — take whichever you prefer, but do not
assume `expires_in` is the 30-day device TTL: on a share-scoped instance the
token expires with the share, so a one-hour share returns `expires_in: 3600`.
A client that hardcodes the TTL will cache a token that is already dead.

Any failure — wrong code, expired code, already redeemed — returns a bare
`401`. The reason is deliberately not distinguished, so a caller cannot use the
error to probe which codes exist.

Pairing parameters, all enforced server-side:

- Code is **6 characters** from Crockford base32
  (`0123456789ABCDEFGHJKMNPQRSTVWXYZ` — no `I`, `L`, `O` or `U`, so it survives
  being read aloud), single-use, **TTL 10 minutes**.
- **Comparison is case-insensitive and whitespace-trimmed.** Uppercase it for
  display; accept whatever the user types.
- Attempts are **rate-limited per source address**, and a rate-limited attempt
  returns the same bare **`401`** as a wrong code — not a `429`. That is
  deliberate: a distinguishable response is an oracle. Back off on repeated
  `401`s rather than waiting for a status that will not come. (`429` exists,
  but on the WebSocket handshake, not here.)
- Device token is 32 random bytes, base64url-encoded.
- **Sliding 30-day expiry**: every successful use extends `expires_at`. A device
  used weekly never needs re-pairing; one left in a drawer expires.
- Maximum 32 paired devices, LRU-evicted.

### Revocation

`herdr-expose devices` lists devices with user-agent and last-seen IP — never
the token or its hash — and `herdr-expose devices --revoke <id>` revokes one.
(`herdr-expose status` also lists them, as part of a wider report.) A revoked
device's next request gets `401`. Its **already-open WebSocket is not
proactively closed** with an application code — see
[Reconnection](#reconnection) for how a client detects this reliably.

### Browser-specific hardening

Clients do not need to do anything for these, but they explain the responses:

- The request `Origin` is checked against an allowlist **before** any CORS header
  is echoed back. An unapproved origin gets no CORS headers at all.
- `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`.
- The WebSocket handshake is rate-limited.

---

## 4. The WebSocket: two planes

```
GET /v1/stream?token=<token>        (or Authorization: Bearer)
Upgrade: websocket
```

One socket carries two kinds of message, distinguished by the **WebSocket
opcode**. There is no negotiation, no capability handshake and no fallback — a v1
server always speaks both.

| Plane | Opcode | Encoding | Carries |
|---|---|---|---|
| **Control** | TEXT | JSON | tree, state, commands, results, lifecycle |
| **Data** | BINARY | fixed header plus raw bytes | terminal output and input |

The full inventory, and it is exactly this — there is nothing else on the wire:

```
control (JSON text)
  client -> server   hello · subscribe · unsubscribe · viewport · resize ·
                     repaint · scroll · seen · command · ping
  server -> client   welcome · tree · agent · geometry · transcript ·
                     closed · result · error · pong

data (binary)
  server -> client   1 = frame · 2 = snapshot · 3 = gap
  client -> server   16 = input
```

**`snapshot` and `gap` are binary-only and never JSON.** Both must stay strictly
ordered against the output stream they refer to; a `gap` that arrives out of
order relative to the bytes it describes is worse than no `gap` at all.

**Terminal bytes are never base64 and never JSON.** Base64 exists at exactly one
place in the whole system — decoding Herdr's own NDJSON inside the server — and
never touches the wire.

**Compression.** Enable `permessage-deflate`. The server uses level 3 with
context takeover on and a 1024-byte threshold, so small messages (keystrokes,
short echoes) skip compression entirely while full-screen redraws get the ~10x
ratio that makes a tunnel usable on cellular. Most WebSocket libraries negotiate
this for you; do not disable it.

**Backpressure, client side.** If your outbound buffer exceeds ~256KB, drop
scroll/wheel input. **Never drop keystrokes.** Degrade scrolling, never typing.

---

## 5. Control plane — JSON frames

Every control frame:

```json
{ "seq": 41, "type": "tree", "data": { } }
```

- `seq` is a `uint64`, monotonic **per connection**, assigned by the server **at
  write time** across both planes from one counter. **Clients send `seq: 0`** (or
  omit it); an inbound `seq` is parsed and ignored.
- `type` is a lowercase string.
- `data` is an object, always present, possibly empty.

**Clients MUST ignore unknown `type` values and unknown fields inside `data`.**
This is how the API grows without a version bump.

### Client to server

#### `hello`

First message after the socket opens. The server will not send `tree` until it
arrives.

```json
{ "type": "hello", "data": {
  "client": "herdr-ios",
  "client_version": "1.0.0",
  "protocol": 1,
  "capabilities": ["summary_html"]
}}
```

#### `subscribe` / `unsubscribe`

Declare interest in targets. A `target` is a **session-qualified** Herdr pane
identifier, ASCII, exactly as it appears in `tree`: `<session>/<pane_id>`.

**`subscribe` and `viewport` are the same message.** The server handles them
identically, and `data.targets` is an **object mapping target to render mode**,
not an array. A `subscribe` carrying an array is parsed as an empty map and
does nothing at all — silently, with no error frame — which is the single
easiest way to build a client that connects, renders a tree and then shows a
blank pane forever.

```json
{ "type": "subscribe", "data": { "targets": {
  "herdr-plugins/w1:p1": "transcript",
  "crypto-desk/w1:p1":   "summary"
}}}
```

The map is the **complete** statement of what this connection is rendering;
anything absent from it is dropped to `none`. `unsubscribe` is sugar for
setting those targets to `none`, and it is the one place a plain array is
correct:

```json
{ "type": "unsubscribe", "data": { "targets": ["crypto-desk/w1:p1"] } }
```

#### `resize` — NOT required, and it mutates the pane for everyone

```json
{ "type": "resize", "data": { "target": "herdr-plugins/w1:p1", "cols": 80, "rows": 24 } }
```

> **Looking must not touch.** Earlier drafts of this document said `resize` was
> required before the first frame and that a target with no geometry produced no
> output. **Both were withdrawn** (SPEC AMENDMENTS 14). They were what made
> merely opening a pane in a browser declare a size for it — SIGWINCHing the
> owner's agent into throwing away the screen you opened it to read, under their
> hands, while they were typing in it. `welcome.geometry.resize_required` is
> `false` on the wire for exactly this reason.

- **You do not send `resize` to view a pane.** A `live` attach passes no size
  upstream, Herdr uses the pane's own geometry and reports it, and the server
  relays it to you as a [`geometry`](#geometry) control frame. Render **at** that
  grid and scale your font to fit your box.
- `resize` is the **explicit, user-initiated "fit this pane to my window"
  action**, and it is the only thing in this product that changes a pane for
  every client attached to it, including the owner's local terminal.
  `welcome.geometry.resize_mutates_pane` is `true`. **Confirm with the user
  before you send one.**
- **`{"match": true}` gives the geometry back** to the pane — it is the undo, and
  it is how you leave a pane as you found it. A zero or negative `cols`/`rows`
  means the same thing.

```json
{ "type": "resize", "data": { "target": "herdr-plugins/w1:p1", "match": true } }
```

- **Floor: 20 cols by 6 rows.** Smaller values are clamped.
- Geometry is **per connection**. Your phone at 40 columns does not resize the
  laptop looking at the same pane; each connection gets its own upstream stream
  with its own size.
- A `resize` for a `transcript` target is **dropped**: a transcript declares no
  geometry, and that is the entire point of the mode.
- Geometry must be derived from **measured cell metrics**, not from a CSS
  transform and not from browser zoom. Client-local font size must never change
  the PTY size.

#### `viewport`

Declare what you are **rendering**. Identical in shape and handling to
`subscribe` — the two names exist for readability, not for different behaviour.
This is a statement, not a request; see
[section 8](#8-viewport-modes-and-geometry).

```json
{ "type": "viewport", "data": { "targets": {
  "herdr-plugins/w1:p1": "live",
  "herdr-plugins/w1:p2": "summary",
  "crypto-desk/w3:p1":   "transcript"
}}}
```

Valid modes are `live`, `transcript`, `summary` and `none`. **An unrecognised
mode string is read as `none`**, so a typo is a silently blank pane — the
server never rejects it. The modes are also enumerated on the wire in
`welcome.viewport.modes`, which is the authoritative list.

#### `repaint`

Ask the server for a guaranteed full repaint of a **live** target.

```json
{ "type": "repaint", "data": { "target": "herdr-plugins/w1:p1" } }
```

You send this when you have **measured** that your own rendering is out of
alignment — geometry drift, a webfont that loaded after the grid was sized, a
DPR or zoom change, a wrapped buffer that disagrees with the current width — and
you are about to reset your emulator. The server replies with a `snapshot`
(binary type 2). It is a no-op for a target that is not `live`: a `transcript`
has no grid to be misaligned with.

Rate-limit yourself. The server does not throttle this, and a repaint loop is
indistinguishable from flicker.

#### `scroll`

Scroll a target's **own** buffer, for a client that holds control.

```json
{ "type": "scroll", "data": { "target": "herdr-plugins/w1:p1", "delta": -10 } }
```

`delta` is in lines; negative scrolls back. On failure the server replies with
an [`error`](#error) frame carrying `code: "scroll_failed"` — there is no
`result`, because a `scroll` carries no `id`.

**Scroll is dropped under backpressure.** If your connection is backlogged the
server discards `scroll` and keeps delivering keystrokes. That is the policy in
both directions: degrade scrolling, never typing.

A viewer with no control scrolls their **own** local scrollback and sends
nothing. `pane.scroll` moves the shared viewport for everybody attached — it is
not on the read path and must not be put there.

#### `seen`

Clear the `done` badge for one target, **for this connection only**.

```json
{ "type": "seen", "data": { "target": "herdr-plugins/w1:p1" } }
```

This is the **only** way to mark a pane seen, and a fresh `tree` follows
immediately with the recomputed `done`. Send it when the user actually looks at
a pane.

**Do not use `pane.focus` or `agent.focus` for this.** Herdr's own focus marks
panes seen *globally*, which would wipe the machine owner's Done badges from
under them — a remote viewer glancing at a pane is not the owner having dealt
with it. Unseen state lives per connection inside this server and never leaves
it.

#### `command`

Call a Herdr socket method **on one session's socket**. See
[section 9](#9-calling-herdr-methods).

```json
{ "type": "command", "data": {
  "id": "c-17",
  "session": "herdr-plugins",
  "method": "pane.send_text",
  "params": { "pane_id": "herdr-plugins/w1:p1", "text": "ls" }
}}
```

Session selection, in order:

1. the explicit `session` field;
2. otherwise the session prefix on a target-ish param (`target`, `pane_id`,
   `tab_id`, `workspace_id`, `from`, `to`, `source_pane_id`, `target_pane_id`);
3. otherwise the session of the target this connection is currently rendering
   live;
4. otherwise the server's default session (`targets.default_session` in
   `welcome`).

Herdr knows nothing about the namespacing, so the server strips the resolved
`<session>/` prefix off those params before the call goes upstream — sending
either the qualified or the bare id works. The `result` frame echoes the
`session` it was dispatched to.

#### `ping`

```json
{ "type": "ping", "data": { "t": 1789459200123 } }
```

Echoed back as `pong` with the same `t`. Use it for an RTT-based degraded
indicator. Send every 15 to 30 seconds; it also keeps intermediaries from idling
the socket out.

### Server to client

#### `welcome`

First server message after `hello`.

`welcome` is **self-describing on purpose**: the framing, the limits, the
geometry contract and the render modes are all stated on the wire so a
hand-written client never has to infer them from this page. Read them from
`welcome` rather than hardcoding the values below.

```json
{ "seq": 1, "type": "welcome", "data": {
  "protocol": "v2",
  "api": "2",
  "session": "c_01HQ8R2K9M",
  "connection_id": "c_01HQ8R2K9M",
  "server_version": "0.1.0",
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "identity": { "kind": "device", "name": "Pixel 8" },
  "binary": {
    "server_header": "type:u8 seq:u64be tlen:u16be target:ascii payload:raw",
    "client_header": "type:u8 tlen:u16be target:ascii payload:raw",
    "types": { "frame": 1, "snapshot": 2, "gap": 3, "input": 16 }
  },
  "limits": { "min_cols": 20, "min_rows": 6, "max_write_bytes": 65536 },
  "geometry": {
    "frame": "geometry",
    "defaults_to_pane": true,
    "resize_required": false,
    "resize_mutates_pane": true,
    "match_supported": true,
    "sources": ["pane", "client"]
  },
  "viewport": {
    "modes": ["live", "transcript", "summary", "none"],
    "transcript": {
      "plane": "control", "frame": "transcript", "geometry": false,
      "ansi_stripped": true, "interval_ms": 1000, "sends_on_change": true,
      "sources": ["recent_unwrapped", "detection"], "is_screen_buffer": true
    }
  },
  "scope": "",
  "targets": {
    "format": "<session>/<pane_id>", "separator": "/",
    "multi_session": true, "default_session": "herdr-plugins"
  }
}}
```

- `protocol` is **this wire's** version (`"v2"`); `herdr_protocol` is
  **upstream Herdr's** (`22`). They are unrelated numbers and confusing them is
  a classic first-day bug.
- `session` and `connection_id` are the same value — this connection's id, not a
  Herdr session name.
- `identity.kind` is `device` for a paired client and `local` for one admitted
  by the loopback bypass.
- **`geometry.resize_required` is `false` and `geometry.resize_mutates_pane` is
  `true`.** Honour both; see [`resize`](#resize--not-required-and-it-mutates-the-pane-for-everyone).
- `scope` is non-empty only on a share-scoped instance, and names the one
  session it can see.
- There is **no `features` array in `welcome`** — an earlier draft claimed one.
  Capability discovery is the `binary` / `geometry` / `viewport` / `targets`
  objects above.

Surface `herdr_version` somewhere in your UI. When Herdr is upgraded under a
client, this is the only visible sign before things start parsing oddly.

#### `tree`

The authoritative structure: workspaces, tabs, panes, agents. Sent once after
`welcome`, then on every change. **Render exactly what is here.** Do not derive
structure client-side; two clients showing different trees is a server bug.

```json
{ "seq": 2, "type": "tree", "data": {
  "rev": 41,
  "connected": true,
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "focused_session": "herdr-plugins",
  "focused_pane": "herdr-plugins/w2:p1",
  "sessions": [{
   "id": "herdr-plugins",
   "name": "herdr-plugins",
   "running": true,
   "connected": true,
   "focused": true,
   "origin": true,
   "focused_pane": "herdr-plugins/w2:p1",
   "workspaces": [{
    "id": "herdr-plugins/w1",
    "label": "herdr-expose",
    "cwd": "/Users/me/src/herdr-expose",
    "tabs": [{
      "id": "herdr-plugins/w1:t1",
      "label": "build",
      "panes": [{
        "id": "herdr-plugins/w1:p1",
        "terminal_id": "term_8f21",
        "title": "claude",
        "cwd": "/Users/me/src/herdr-expose",
        "agent": "claude",
        "status": "working",
        "idle": false,
        "done": false,
        "focused": true,
        "cols": 120,
        "rows": 40,
        "alive": true,
        "mode": "live"
      }]
    }]
   }]
  }]
}}
```

Notes that matter for a client:

- **A session `id` IS stable** (it is the session name). A pane `id` is not
  stable across a Herdr server restart, while `terminal_id` is stable across
  moves — key your UI state on `<session>/<terminal_id>` where you can.
- A session with `connected: false` has no `workspaces`; keep its row and show
  it as offline. It reconnects on its own, and its entry fills back in.
- `status` is one of `idle`, `working`, `blocked`, `unknown`. `blocked` means the
  agent is waiting on a human — that is your cue for the Q&A view.
- `idle` is a fact about the pane. **`done` is a fact about you** — see below.
- `mode` is the mode the **server** chose for you. It may not be what you asked
  for in `viewport`.
- `connected: false` at the TOP level means every session is unreachable; a
  per-session `connected: false` means only that one is. Either way we are
  retrying. Keep
  the client connection open and show a degraded state; a fresh `tree` follows
  reconnection.

#### `agent`

Agent status transitions, ahead of the next full `tree`.

```json
{ "seq": 87, "type": "agent", "data": {
  "target": "herdr-plugins/w1:p1",
  "session": "herdr-plugins",
  "status": "blocked",
  "detection": "Do you want to proceed? (y/n)"
}}
```

`detection` is present **only when `status` is `blocked`** — do not expect it on
other transitions, and do not render an empty Q&A view when it is absent.

**Every known agent is emitted as an `agent` frame immediately after the first
`tree`**, so a freshly connected client has complete agent state without asking
for it. After that, frames arrive on transitions.

`detection` is the raw text Herdr classified on. Render it as-is; **do not parse
per agent kind** — that is a maintenance treadmill and it breaks on every agent
update. Pair it with a fixed key bar (y / n / enter / esc / arrows / 1 2 3).

**`done` is per connection, and the server derives it for you.** It is computed
as:

```
done = idle AND unseen-by-THIS-connection
```

The server keeps one unseen set per connection, so **two clients can legitimately
receive different `done` values for the same pane at the same instant**, and that
is correct: with a laptop and two phones attached, a global seen set would mean
whoever glances first wipes everyone else's badges.

Client rules:

- **Render `done`. Do not recompute it**, and do not cache it as a property of
  the pane shared across connections.
- A pane is marked seen for your connection when you send the
  [`seen`](#seen) control frame — reads do not mark seen, and neither does
  rendering. **Never call `pane.focus` / `agent.focus` to do it**: Herdr's focus
  marks the pane seen globally and would wipe the machine owner's own badges.
- A new connection starts with currently-idle panes already seen, so you will not
  open to a wall of stale badges.

#### `result`

Response to a `command`, correlated by `id`.

```json
{ "seq": 88, "type": "result", "data": {
  "id": "c-17", "session": "herdr-plugins", "ok": true, "result": { "written": 3 }
}}
```

```json
{ "seq": 89, "type": "result", "data": {
  "id": "c-18", "session": "herdr-plugins", "ok": false,
  "error": "unknown or disconnected herdr session: crypto-desk"
}}
```

**`error` on a `result` is a plain STRING, not an object.** There is no `code`
and no `message` field to read — an earlier draft of this document claimed a
`{code, message}` object with a fixed code vocabulary, and a client that does
`result.error.message` gets `undefined`. Render the string.

`result.result` is Herdr's own reply, passed through verbatim and unvalidated.
`result.session` echoes the session the call was dispatched to, which is worth
showing when you did not name one explicitly.

#### `closed`

A target is gone: the pane exited, or control was lost.

```json
{ "seq": 120, "type": "closed", "data": { "target": "herdr-plugins/w1:p1", "reason": "exited" } }
```

**`reason` is free text for a human, not an enum.** It is Herdr's own close
reason, or this server's explanation of why a stream could not be kept alive
(e.g. `upstream stream could not be kept alive after 5 restarts: ...`). An
earlier draft of this document listed four fixed values; do not switch on it.
Stop rendering that target, show the reason, and expect a `tree` reflecting the
removal.

#### `geometry`

The size a `live` target is actually being streamed at, and **where that size
came from**. Sent when a stream attaches and whenever the size changes.

```json
{ "seq": 44, "type": "geometry", "data": {
  "target": "herdr-plugins/w1:p1",
  "cols": 120,
  "rows": 40,
  "source": "pane"
}}
```

- `source: "pane"` — we attached with **no** size, Herdr used the pane's own
  geometry and reported it back. **Nothing of the user's moved.** This is the
  normal case, and it is what makes opening a pane non-destructive.
- `source: "client"` — somebody explicitly sent a `resize`, so the pane is now
  that size **for everyone attached to it**, including the owner's local
  terminal.

Render at `cols` x `rows` and scale your font to fit your box. Never resize the
pane to fit the browser. Surfacing `source` in the UI is worth it: it is the
difference between "you are watching" and "you moved somebody's terminal".

#### `error`

An out-of-band failure on a message that carried no `id`, so there is no
`result` to put it in.

```json
{ "seq": 91, "type": "error", "data": {
  "target": "herdr-plugins/w1:p1",
  "code": "input_failed",
  "detail": "target is not live"
}}
```

Current codes are `input_failed` (binary input to a target that cannot take it —
a `transcript` target refuses raw bytes; use `agent.prompt` or `agent.send_keys`
instead) and `scroll_failed`. **More will appear.** Show `detail` and carry on;
never treat an unknown `code` as fatal.

#### `pong`

```json
{ "seq": 90, "type": "pong", "data": { "t": 1789459200123, "server_t": 1789459200140 } }
```

`t` is **your** value echoed back verbatim, so RTT is `now - t` and needs no
clock synchronisation. `server_t` is the server's wall clock in milliseconds,
for a clock-skew readout; do not use it to compute RTT. Show a degraded
indicator above ~250ms rather than pretending everything is fine.

---

## 6. Data plane — binary frames

Terminal bytes travel in WebSocket **binary** frames with a fixed header. It is
deliberately trivial: any language parses it in about ten lines, with no codegen,
no schema compiler and no dependency. That is the whole reason it is not
protobuf.

### Server to client

```
 byte:  0        1                     9            11              11+tlen
        +--------+---------------------+------------+---------------+---------+
        | type   | seq                 | tlen       | target        | payload |
        | u8     | u64 big-endian      | u16 BE     | ASCII, tlen B | raw     |
        +--------+---------------------+------------+---------------+---------+
```

| `type` | Name | Payload |
|---|---|---|
| `1` | `frame` | raw ANSI bytes from the pane |
| `2` | `snapshot` | raw ANSI bytes: a full repaint, and a signal that **your buffer cannot be trusted** — reset the emulator and repaint |
| `3` | `gap` | exactly 8 bytes: `u64` big-endian `bytes_dropped` |

- **All integers are big-endian** (network byte order): `seq` as u64 BE, `tlen`
  as u16 BE, and `bytes_dropped` in a `gap` payload as u64 BE.
- `target` is ASCII, exactly `tlen` bytes, no terminator.
- `payload` is **everything after the target** — its length is the frame length
  minus `11 + tlen`. There is no payload-length field; the WebSocket frame
  already carries the length. Do not look for one.
- `payload` is **raw terminal bytes**. Feed them straight to your emulator. Do
  not decode them, and do not assume UTF-8 boundaries: a multi-byte character can
  and will be split across two frames. Your emulator handles that; a conversion
  to a native string type does not.
- Maximum payload is 64KB; longer output arrives as multiple frames.

### Worked example

> The hex below uses the short target `w1:p1` to keep the byte counts readable.
> **On the wire every target is session-qualified** (`herdr-plugins/w1:p1`), so
> a real frame's `tlen` is larger by `len(session) + 1`. Nothing else changes.

A `frame` on target `w1:p1` carrying the four bytes `l`, `s`, CR, LF, with
`seq = 258`:

```
01  00 00 00 00 00 00 01 02  00 05  77 31 3a 70 31  6c 73 0d 0a
^   ^                        ^      ^               ^
|   |                        |      |               payload, 4 bytes
|   |                        |      target "w1:p1", 5 bytes
|   |                        tlen = 0x0005 = 5
|   seq = 0x0000000000000102 = 258
type = 1 (frame)
```

Total 20 bytes: 1 + 8 + 2 + 5 + 4.

A `gap` announcing 131072 dropped bytes on the same target:

```
03  00 00 00 00 00 00 01 03  00 05  77 31 3a 70 31  00 00 00 00 00 02 00 00
                                                    ^
                                                    bytes_dropped = 0x20000
```

A `snapshot` always follows a `gap` for the same target.

**Where snapshots come from.** Herdr's own terminal records carry a `full` flag
marking a full repaint. It is set on attach and after a resize, but ALSO
periodically on a completely idle pane, so `full` on its own does not mean your
buffer is stale.

Type 2 therefore means exactly one thing: **this connection's buffer cannot be
trusted.** The server sends it for the first full repaint after

- you attach (or re-attach) to the target,
- a `gap` on that target — bytes really were dropped,
- an upstream stream restart, e.g. the observe -> control takeover respawn.

Every other repaint, including Herdr's idle ones, arrives as an ordinary `frame`
(type 1). A full repaint carries its own clear/home sequences, so writing it into
a live buffer is seamless — resetting the emulator on each one is what makes a
terminal visibly flicker. On `snapshot`, reset and write the payload; on `frame`,
just write it.

The first thing you receive after attaching is still a complete screen, so there
is no separate "request a snapshot" call, and you do not need one on reconnect.

### Reference decoder

```js
function decode(buf) {                       // buf: Uint8Array
  const dv   = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const type = dv.getUint8(0);
  const seq  = dv.getBigUint64(1);           // big-endian is the default
  const tlen = dv.getUint16(9);
  const target  = new TextDecoder("ascii").decode(buf.subarray(11, 11 + tlen));
  const payload = buf.subarray(11 + tlen);
  return { type, seq, target, payload };
}
```

```swift
func decode(_ d: Data) -> (type: UInt8, seq: UInt64, target: String, payload: Data) {
    let type = d[0]
    let seq  = d.subdata(in: 1..<9).reduce(UInt64(0)) { ($0 << 8) | UInt64($1) }
    let tlen = Int(d[9]) << 8 | Int(d[10])
    let target = String(decoding: d.subdata(in: 11..<(11 + tlen)), as: UTF8.self)
    return (type, seq, target, d.subdata(in: (11 + tlen)..<d.count))
}
```

### Client to server (input)

Keystrokes are binary too. There is **no JSON encode on the keystroke path** —
that is the latency budget this whole design exists to protect.

```
 byte:  0        1            3              3+tlen
        +--------+------------+---------------+---------+
        | type   | tlen       | target        | payload |
        | = 16   | u16 BE     | ASCII, tlen B | raw     |
        +--------+------------+---------------+---------+
```

| `type` | Name | Payload |
|---|---|---|
| `16` | `input` | raw bytes to write to the pane |

Note there is **no `seq`** on input. Send the exact bytes the terminal should
receive: `0x0d` for Enter, `0x03` for Ctrl-C, `ESC [ A` (`0x1b 0x5b 0x41`) for
Up, and the raw UTF-8 of typed text.

Input for `w1:p1` carrying Ctrl-C:

```
10  00 05  77 31 3a 70 31  03
```

Coalesce only what is already pending. **Never delay a keystroke to batch it.**

---

## 7. Sequence numbers, gaps and reconnection

### `seq`

`seq` is a `uint64`, monotonic per connection, assigned by the server **at write
time**, from one counter shared across both planes. It is for **ordering and
debugging**.

Clients send `seq: 0` on control frames and carry no `seq` at all on binary input
frames. Any `seq` a client sends is parsed and discarded.

**`seq` is not a resume cursor.** Do not persist it. Do not send it back on
reconnect. There is no replay.

### `gap`

Under backpressure — a client too slow, a tunnel stalling — the server drops
buffered output for the affected target rather than growing a buffer without
bound. It then tells you, in order, inline with the data plane:

1. a `gap` frame (type 3) with the dropped byte count,
2. a `snapshot` frame (type 2) with the current screen.

`gap` is a binary frame rather than a control message precisely so it stays
ordered against the output it refers to.

On `gap`: clear that terminal and apply the snapshot. Optionally flash a marker.
Losing output under load is acceptable; silently lying about it is not.

### Reconnection

**There is no resume. A reconnecting client repaints.**

```
1. socket closes
2. back off: 0.5s, 1s, 2s, 4s, 8s, capped at 30s, with jitter
3. reconnect with the same device token
4. hello -> welcome -> tree
5. subscribe + resize for what you are showing
6. a snapshot arrives per target; clear and repaint
```

A terminal has no perceivable missed bytes once you repaint. Scrollback written
while you were disconnected stays in Herdr — the user can scroll to it — but it
is not re-sent. Do not build a client that waits for a replay; it is never
coming.

Your unseen set (for DONE) is client-side and survives your own reconnect
whenever you choose to keep it. A brand-new install should treat currently-idle
panes as already seen, so the app does not open with a wall of stale badges.

**Close codes.** Do not build logic on them. Authentication is enforced
**before** the upgrade — a bad, revoked or expired token gets an HTTP `401` and
the socket is never established — and the server closes a live connection
abruptly rather than sending an application close code. There is no `4401` and
no `4429` on this wire today, despite an earlier draft of this document
promising both.

So the reliable way to tell "re-pair" from "network down", on every reconnect,
is: **re-read `GET /v1/config` and look at `auth_required` / `authenticated`.**
That endpoint needs no auth precisely so this check always works. `401` from it,
or `authenticated: false` while `auth_required: true`, means wipe the stored
token and return to pairing.

---

## 8. Viewport modes and geometry

You tell the server what you are **rendering**. The server decides what it
**sends**. This is the mechanism that stops twenty live panes from stalling a tab
and pinning a laptop's CPU, and it is not negotiable from the client side.

| Mode | The server sends | Cost |
|---|---|---|
| `live` | full terminal stream, same-tick coalescing, 64KB flush | one upstream stream, per connection |
| `summary` | periodic `snapshot` frames at 1-2 Hz | shared across clients |
| `transcript` | `transcript` **control-plane** frames at ~1Hz, on change only | shared across clients, **no geometry** |
| `none` | nothing but control-plane state | free |

**`transcript` is the right default for every pane, agent or not** (SPEC
AMENDMENTS 14 K2). A shell is output like any other output and reads fine as
text, and a transcript never attaches to the pane, so opening one is provably
non-destructive. Offer `live` as an opt-in the user takes deliberately, having
been told in one line what it costs — that is what the shipped web UI does
(consent per pane, per session). A client that opens everything `live` by
default recreates the exact problem this mode exists to prevent.

Rules a client must honour:

1. **You may not request `live` directly and expect it.** You declare `live` for
   what you are rendering full-size; the server grants it for the focused pane.
2. **Render the mode you were given**, from the `mode` field in `tree`. Expect to
   be demoted.
3. **`summary` is not a terminal.** Render those snapshots as lightweight
   pre-rendered ANSI-to-HTML or a canvas blit. **Never instantiate one terminal
   emulator per tile** — twenty emulator instances is what kills a browser tab.
   One emulator, for the `live` pane, is the correct budget.
4. **`live` streams are per connection**, each with the geometry you set via
   `resize`. `summary` reads are geometry-free and therefore deduplicated across
   clients — which is why a summary tile shows the pane at its own size, not
   yours.
5. **`transcript` declares NO geometry and must never be paired with a
   `resize`.** That is the whole point of the mode. `live` attaches an observer
   at your size, which SIGWINCHes an agent TUI into throwing its screen away and
   redrawing — so on a phone, opening a pane used to destroy the history you
   opened it to read. A transcript subscriber never attaches, so `viewport_rows`
   for that pane is provably unchanged by your being there. The server enforces
   this: a `resize` for a transcript target is dropped, and raw binary input to
   one is refused with `input_failed` (use `agent.prompt` / `agent.send_keys`).

### The `transcript` frame

```json
{ "seq": 41, "type": "transcript", "data": {
  "target": "herdr-plugins/w1:p1",
  "source": "recent_unwrapped",
  "text": "\u276f explain what a PTY is\n\n\u23fa A PTY is ...",
  "lines": 400,
  "truncated": true,
  "agent": true,
  "state": "idle",
  "at": "2026-09-19T20:31:02+05:30"
}}
```

- `text` is plain UTF-8 with every escape sequence stripped **server-side**. It
  is not a binary frame and must not be fed to a terminal emulator.
- It is sent **only when the text actually changed**, so an idle pane costs
  nothing. A new subscriber always gets one immediately.
- `source` is `recent_unwrapped` normally and `detection` while the agent is
  blocked — Herdr's own spellings, reported so you can say what you are showing.
- **This is the agent's visible SCREEN, not its message history.** Herdr exposes
  the screen; there is no conversation log to read. Render the text; do not
  invent message boundaries, roles or turns from it.

---

## 9. Calling Herdr methods

> ### If you are talking to the Herdr socket yourself
>
> You do not need this to use *our* API — we handle it — but if you are writing
> anything that speaks to Herdr 0.9.0 directly, these three facts are
> undocumented upstream and each one costs a day:
>
> 1. **The socket serves exactly ONE request per connection, then closes it.**
>    Pipelining a second request on the same connection gets a broken pipe. Dial
>    a fresh unix connection per call. Only `events.subscribe` holds one open,
>    for the life of the subscription. Every instinct you have about connection
>    reuse is wrong here, and the failure surfaces on the *second* call, which is
>    not where you will look.
> 2. **`session.snapshot` returns the whole tree in one call** — use it instead
>    of composing `workspace.list` + `tab.list` + `pane.list` + `agent.list`.
> 3. **Three of the 27 subscription kinds are per-pane and require a `pane_id`**:
>    `pane.agent_status_changed`, `pane.output_matched`, `pane.scroll_changed`.
>    Subscribing to all 27 at once fails the **entire call** with
>    `missing field pane_id` — not just those three. Subscribe to the 24
>    session-wide kinds; `pane.updated` covers status changes globally.
>
> And subscribe **before** you snapshot: 0.9.0 does not replay retained event
> history, so snapshot-then-subscribe silently loses the gap between them.

`command` is a straight pass-through to the Herdr socket API. **Method names are
not enumerated or validated here** — whatever Herdr 0.9.0 accepts (102 request
methods), you can send. Run `herdr api schema` for the authoritative list; this
server deliberately does not maintain a second copy of it that could drift.

Commonly useful:

| Method | Purpose |
|---|---|
| `pane.send_text` | type into a pane |
| `pane.read` | read a pane's buffer (`visible` or `scrollback` source) |
| `pane.scroll` | scroll the buffer (0.9.0+) |
| `pane.selection.read` | read the current selection (0.9.0+) |
| `pane.link.activate` | activate a detected link (0.9.0+) |
| `agent.read` | read the agent detection region |
| `agent.prompt` | send a prompt to an agent |
| `command.invoke` | invoke a Herdr command (0.9.0+) |

```json
{ "type": "command", "data": {
  "id": "c-31",
  "method": "agent.read",
  "params": { "pane_id": "herdr-plugins/w1:p1", "source": "detection" }
}}
```

`id` is yours: any string, unique within the connection. The matching `result`
carries it back. Requests without an `id` get no `result`.

> **This is the RCE surface.** `pane.run` and `agent.prompt` execute commands as
> the user. Anything your UI exposes here, a compromised session can reach.

---

## 10. Errors

HTTP:

| Status | Meaning |
|---|---|
| `400` | malformed request |
| `401` | missing, invalid, expired or revoked token |
| `403` | origin not allowlisted, or `Host` did not match the pinned value |
| `404` | no such route |
| `405` | wrong method (a non-`POST` to `/v1/pair`) |
| `429` | rate-limited — the **WebSocket handshake** only; a rate-limited pairing attempt returns `401` |

HTTP error bodies are **plain text**, not JSON — `unauthorized`,
`forbidden origin`, `forbidden host`, `origin required`, `bad request`,
`method not allowed`. Read the status code; the body is for a human reading a
curl transcript. Do not write a JSON parser for it, and in particular do not
expect an `{"error": {...}}` envelope, which an earlier draft of this document
showed.

There is no `503`: when the Herdr socket is down the HTTP surface still answers
and the condition shows up as `connected: false` in `tree` and `upstream: false`
in an authenticated `/healthz`.

Control plane, two shapes and they are **not** the same:

| Frame | Field | Type | When |
|---|---|---|---|
| `result` with `"ok": false` | `error` | **string** | a `command` failed — it carried an `id`, so the failure comes back correlated |
| `error` | `code` + `detail` | strings | `scroll` or binary `input` failed — neither carries an `id`, so there is nowhere else to put it |

A `result.error` string is either this server's own message (scope refusal,
`unknown or disconnected herdr session: <name>`, a 15-second call timeout) or
Herdr's error text passed through verbatim. **There is no code vocabulary and
no machine-readable classification.** Match on it at your peril; show it to the
user instead.

`error` frame codes today are `input_failed` and `scroll_failed`. More will
appear. Show `detail` and carry on.

A **scoped instance** (`herdr-expose share`) refuses out-of-scope methods and
out-of-scope sessions here, as an `ok: false` result. That refusal is the
server enforcing the share's boundary — it is not a transient failure and
retrying will not help.

---

## 11. Versioning and compatibility

**The API is the product.** The web UI is its first client, a mobile app will be
its second, and the contract is what makes the second one a weekend rather than a
rewrite.

### The promise

For as long as `/v1` is served:

1. **No existing control-frame `type` is removed or renamed.**
2. **No existing field is removed, renamed, or changes type or meaning.**
3. **No existing binary `type` byte changes meaning, and the 11-byte header
   layout does not change.** The data plane is frozen by design.
4. **No new field becomes mandatory** in a client-to-server message.
5. **Auth and pairing semantics do not change** in a way that invalidates a
   working device token.

### What may change without notice

- **New control-frame `type` values.** Ignore what you do not know.
- **New fields** in existing `data` objects. Ignore what you do not know.
- **New `type` bytes** in the binary plane. **Ignore any binary frame whose type
  byte you do not recognise** — the header is fixed, so you can always skip it
  cleanly.
- **New fields on `/v1/config` and `welcome`**, and new values of `mode`. There
  is no `features` array; capabilities are announced as new fields.
- Anything under `/*` — the embedded web app is not API surface.

### Client requirements

A conforming client **must**:

- ignore unknown control-frame types, unknown JSON fields, and unknown binary
  type bytes;
- not depend on `seq` values being contiguous, or on resuming from one;
- not depend on pane `id` stability across Herdr restarts (use `terminal_id`);
- send `resize` before expecting output, and re-send it when geometry changes;
- treat close code `4401` as re-pair, not retry.

### Breaking changes

A breaking change is `/v2`, served **alongside** `/v1` from the same binary. `v1`
keeps working. When `v2` exists it will be announced in the `api` field of
`/v1/config` and `welcome`, so a client can detect it without a probe.

The likely shape of `v2` is protobuf on both planes, and it will happen only when
a second independently-built client makes codegen pay for itself, not before.
Until then this document is the schema.

---

## 12. A minimal client, end to end

```js
// 1. bootstrap. /v1/config needs NO auth - it is how you find out whether auth
//    is required at all. Send the token anyway if you have one, so that
//    `authenticated` means something.
const auth = token ? { Authorization: `Bearer ${token}` } : {};
const cfg  = await fetch("/v1/config", { headers: auth }).then(r => r.json());

// These two fields are the only way to tell "pair me" from "network down":
// a browser cannot read the status of a REJECTED WebSocket upgrade.
if (cfg.auth_required && !cfg.authenticated) return showPairingScreen();

// lan mode is plain HTTP on a LAN IP: not a secure context, so no service
// worker and no install prompt. There is no secure_context field on /v1/config
// - ask the browser.
if (!window.isSecureContext) hideInstallPrompt();

// 2. connect
const q  = token ? `?token=${token}` : "";
const ws = new WebSocket(`${location.origin.replace(/^http/, "ws")}/v1/stream${q}`);
ws.binaryType = "arraybuffer";

ws.onopen = () => send({ type: "hello", data: { client: "demo", protocol: 1 } });

let grid = {};                                  // target -> {cols, rows}

ws.onmessage = (ev) => {
  if (typeof ev.data === "string") {
    const msg = JSON.parse(ev.data);
    switch (msg.type) {
      case "welcome":  readContract(msg.data); break;  // limits, modes, framing
      case "tree":     render(msg.data); break;        // render done, never recompute
      case "agent":    agentUpdate(msg.data); break;
      case "geometry": // the PANE's size, reported to us. Render AT it.
                       grid[msg.data.target] = msg.data; fitFont(msg.data); break;
      case "transcript": showText(msg.data); break;    // plain text, NOT for an emulator
      case "pong":     rtt = Date.now() - msg.data.t; break;
      case "closed":   drop(msg.data.target); break;
      case "result":   resolve(msg.data.id, msg.data); break;
      case "error":    toast(msg.data.code, msg.data.detail); break;
      // anything else: ignore, on purpose
    }
    return;
  }
  const f = decode(new Uint8Array(ev.data));
  if (f.type === 1) term.write(f.payload);                        // frame
  else if (f.type === 2) { term.reset(); term.write(f.payload); }  // snapshot
  else if (f.type === 3) markGap(f.target);                       // gap
  // unknown type byte: ignore
};

// 3. attach to a pane. `targets` is an OBJECT of target -> mode. An array is
//    parsed as an empty map and silently does nothing.
//    NO resize: that would move the pane for everyone attached, including the
//    person sitting at the machine. Wait for the `geometry` frame instead.
function attach(target, mode = "transcript") {
  send({ type: "subscribe", data: { targets: { [target]: mode } } });
}

// The ONLY thing that changes a pane for everybody. Ask the user first, and
// offer the undo.
function fitToMyWindow(target, cols, rows) {
  send({ type: "resize", data: { target, cols: Math.max(20, cols),
                                         rows: Math.max(6, rows) } });
}
const giveItBack = (target) => send({ type: "resize", data: { target, match: true } });

// Clear this connection's `done` badge. Never pane.focus - that marks the pane
// seen for the machine's owner too.
const markSeen = (target) => send({ type: "seen", data: { target } });

// 4. type - binary, no JSON on this path
function input(target, bytes) {
  const t = new TextEncoder().encode(target);
  const b = new Uint8Array(3 + t.length + bytes.length);
  const dv = new DataView(b.buffer);
  dv.setUint8(0, 16);
  dv.setUint16(1, t.length);
  b.set(t, 3);
  b.set(bytes, 3 + t.length);
  ws.send(b);
}

const send = (o) => ws.send(JSON.stringify(o));
```

That is a working client, and the three lines most likely to be wrong in one
that is not are all above: `targets` is an **object**, `/v1/config` takes **no
auth**, and you **do not send `resize`** to look at something. Everything
beyond this — summary tiles, the blocked-agent Q&A view, a mobile key bar — is
presentation built on the same message kinds.

---

See also: [`adr/`](adr/) for why each of these decisions was made,
[`troubleshooting.md`](troubleshooting.md) for the failures that look like bugs
and are not, and [`ATTRIBUTION.md`](ATTRIBUTION.md) for prior art this design
learned from.

herdr-expose is MIT-licensed; see [`../LICENSE`](../LICENSE).
