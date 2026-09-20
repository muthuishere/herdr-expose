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
bind address and whether you need a token:

| mode | bind | device token | Origin + Host pinning | secure context |
|---|---|---|---|---|
| `local` | `127.0.0.1` | **not** required | required | yes (`localhost` is exempt) |
| `lan` | `0.0.0.0` | **required** | required | **no** |
| `quick` | `127.0.0.1` + tunnel | **required** | required | yes |
| `cloudflare` | `127.0.0.1` + tunnel | **required** | required | yes |

- **`local`** — the default. Anyone who can reach loopback already has shell on
  the box, so no token is required. Origin and Host pinning replace it, and they
  are strictly enforced: only `http://127.0.0.1:<port>` and
  `http://localhost:<port>` are accepted, a **missing `Origin` on a WebSocket
  upgrade is rejected**, and a `Host` that does not match is rejected. That is
  what defeats DNS rebinding, which is the real attack on a loopback service.
- **`lan`** — bound to `0.0.0.0`, reachable by anyone on the wifi. A device token
  is mandatory. **Plain HTTP on a LAN IP is not a secure context**, so
  `status.secure_context` is `false`, **the service worker does not register and
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

Read `mode` and `secure_context` from `/v1/config`; never infer them.

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
| `GET` | `/healthz` | none | Liveness |
| `GET` | `/v1/config` | required | Client bootstrap. Contains no secrets. |
| `POST` | `/v1/pair` | pairing code | Exchange a pairing code for a device token |
| `GET` | `/v1/stream` | required | WebSocket upgrade |
| `GET` | `/*` | none | The embedded web app |

### `GET /healthz`

Unauthenticated liveness. Use it to decide whether the server is up before
showing a connection error.

```http
GET /healthz HTTP/1.1
```

```json
{ "ok": true, "version": "0.1.0", "herdr_version": "0.9.0" }
```

### `GET /v1/config`

Bootstrap for a connected client. **Never contains a token or any secret.**

```json
{
  "api": "2",
  "server_version": "0.1.0",
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "features": ["binary_frames", "pairing", "expose"],
  "mode": "local",
  "secure_context": true,
  "auth_required": false,
  "public_url": null,
  "limits": {
    "max_frame_bytes": 65536,
    "min_cols": 20,
    "min_rows": 6
  },
  "ui": { "theme": "auto", "default_view": "grid" }
}
```

`mode` is `local`, `lan`, `quick` or `cloudflare`. `secure_context` is `false`
only in `lan` mode — when it is, skip service-worker registration and hide any
install affordance. `auth_required` is `false` only in `local` mode.
`public_url` is the tunnel URL in `quick` and `cloudflare` mode and `null`
otherwise; in `quick` mode it is assigned by the edge at start, so read it, do
not remember it.

`features` is an open-ended array of strings. **Ignore entries you do not
recognise** — new capabilities are announced here rather than by bumping the API
version.

---

## 3. Authentication and pairing

There are **two kinds of secret**, with different jobs.

| | Server token | Device token |
|---|---|---|
| What it authorises | talking to this instance at all | one specific device |
| Where it comes from | generated at first run, shown by `herdr-expose status` | issued by pairing |
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

- Code is **6 characters**, single-use, **TTL 10 minutes**.
- Attempts are **rate-limited per source address**. Expect `429`.
- Device token is 32 random bytes, base64url-encoded.
- **Sliding 30-day expiry**: every successful use extends `expires_at`. A device
  used weekly never needs re-pairing; one left in a drawer expires.
- Maximum 32 paired devices, LRU-evicted.

### Revocation

`herdr-expose status` lists devices with user-agent and last-seen IP — never the
token or its hash — and revokes them individually. A revoked device's next
request gets `401`; its open WebSocket is closed with code `4401`.

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
  client -> server   hello · subscribe · unsubscribe · viewport · resize · command · ping
  server -> client   welcome · tree · agent · closed · result · pong

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

```json
{ "type": "subscribe",   "data": { "targets": ["herdr-plugins/w1:p1", "crypto-desk/w1:p1"] } }
```

```json
{ "type": "unsubscribe", "data": { "targets": ["crypto-desk/w1:p1"] } }
```

#### `resize` — required before the first frame

Terminal geometry, **per target, per connection**.

```json
{ "type": "resize", "data": { "target": "herdr-plugins/w1:p1", "cols": 80, "rows": 24 } }
```

- **Floor: 20 cols by 6 rows.** Smaller values are clamped.
- Geometry is **per connection**. Your phone at 40 columns does not resize the
  laptop looking at the same pane; each connection gets its own upstream stream
  with its own size.
- **A target with no geometry produces no output.** Send `resize` before, or
  immediately after, `subscribe` — before you expect the first frame.
- Send it again on every layout change (rotation, soft keyboard, window resize).
  Coalesce: one `resize` per settled layout, not one per animation frame.
- Geometry must be derived from **measured cell metrics**, not from a CSS
  transform and not from browser zoom. Client-local font size must never change
  the PTY size.

#### `viewport`

Declare what you are **rendering**. This is a statement, not a request — see
[section 8](#8-viewport-modes-and-geometry).

```json
{ "type": "viewport", "data": { "targets": {
  "herdr-plugins/w1:p1": "live",
  "herdr-plugins/w1:p2": "summary",
  "crypto-desk/w3:p1":   "summary"
}}}
```

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

```json
{ "seq": 1, "type": "welcome", "data": {
  "api": "2",
  "server_version": "0.1.0",
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "connection_id": "c_01HQ8R2K9M",
  "features": ["binary_frames", "expose"]
}}
```

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
- A pane is marked seen for your connection when **the user focuses it** — reads
  do not mark seen. Focus travels as a `command`.
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
  "id": "c-18", "ok": false,
  "error": { "code": "CONTROL_HELD", "message": "another client holds control" }
}}
```

#### `closed`

A target is gone: the pane exited, or control was lost.

```json
{ "seq": 120, "type": "closed", "data": { "target": "herdr-plugins/w1:p1", "reason": "exited" } }
```

`reason` is one of `exited`, `released`, `error`, `upstream_lost`. Stop rendering
that target; a `tree` reflecting the removal follows.

#### `pong`

```json
{ "seq": 90, "type": "pong", "data": { "t": 1789459200123 } }
```

RTT is `now - t`. Show a degraded indicator above ~250ms rather than pretending
everything is fine.

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

**Close codes.** `1000` normal, `1001` server shutting down, `4401` token revoked
or expired — wipe the stored token and return to pairing, do not retry — `4429`
rate-limited, back off hard.

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
  "params": { "pane_id": "w1:p1", "source": "detection" }
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
| `429` | rate-limited (pairing attempts, handshakes) |
| `503` | server up, Herdr socket unavailable |

```json
{ "error": { "code": "invalid_token", "message": "token not recognised" } }
```

Control plane, in a `result` with `"ok": false`:

| `code` | Meaning |
|---|---|
| `CONTROL_HELD` | another client holds control; retry with explicit takeover |
| `NO_SUCH_TARGET` | the target is gone (a `tree` update is coming) |
| `UPSTREAM_ERROR` | Herdr returned an error; `message` carries it verbatim |
| `UPSTREAM_DOWN` | the Herdr socket is disconnected; retry after reconnect |
| `BAD_REQUEST` | malformed frame or parameters |

Unrecognised codes will appear over time. Show `message` and carry on.

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
- **New entries in `features`** on `/v1/config` and `welcome`, and new values of
  `mode`.
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
keeps working. When `v2` exists it will be announced in `features` and in
`welcome`, so a client can detect it without a probe.

The likely shape of `v2` is protobuf on both planes, and it will happen only when
a second independently-built client makes codegen pay for itself, not before.
Until then this document is the schema.

---

## 12. A minimal client, end to end

```js
// 1. bootstrap. In local mode cfg.auth_required is false and the header is
//    simply ignored; in lan, quick and cloudflare mode a device token is
//    mandatory.
const auth = token ? { Authorization: `Bearer ${token}` } : {};
const cfg  = await fetch("/v1/config", { headers: auth }).then(r => r.json());

// lan mode is plain HTTP on a LAN IP: not a secure context, so no service
// worker and no install prompt. Check, do not assume.
if (!cfg.secure_context) hideInstallPrompt();

// 2. connect
const q  = token ? `?token=${token}` : "";
const ws = new WebSocket(`${location.origin.replace(/^http/, "ws")}/v1/stream${q}`);
ws.binaryType = "arraybuffer";

ws.onopen = () => send({ type: "hello", data: { client: "demo", protocol: 1 } });

ws.onmessage = (ev) => {
  if (typeof ev.data === "string") {
    const msg = JSON.parse(ev.data);
    switch (msg.type) {
      case "welcome": break;
      case "tree":    render(msg.data); break;   // render done, do not recompute
      case "agent":   agentUpdate(msg.data); break;
      case "pong":    rtt = Date.now() - msg.data.t; break;
      case "closed":  drop(msg.data.target); break;
      case "result":  resolve(msg.data.id, msg.data); break;
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

// 3. attach to a pane - resize BEFORE you expect output
function attach(target, cols, rows) {
  send({ type: "subscribe", data: { targets: [target] } });
  send({ type: "resize",    data: { target, cols: Math.max(20, cols),
                                            rows: Math.max(6, rows) } });
  send({ type: "viewport",  data: { targets: { [target]: "live" } } });
}

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

That is a working client. Everything beyond it — summary tiles, the
blocked-agent Q&A view, a mobile key bar — is presentation built on the same five
message kinds.

---

See also: [`adr/`](adr/) for why each of these decisions was made, and
[`ATTRIBUTION.md`](ATTRIBUTION.md) for prior art this design learned from.
