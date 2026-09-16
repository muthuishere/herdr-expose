# herdr-expose API v1

This document is the **public contract** for herdr-expose. It is written so that
a Swift, Kotlin, Rust or Python client can be built from this page alone, with
no access to the server's source code. If something a client needs is not here,
that is a bug in this document — please report it.

The web UI shipped inside the binary is simply the first client of this API. It
has no privileged path, no private endpoints, and no frame types that are not
documented below.

- **Base URL**: `http://127.0.0.1:21118` by default (port is configurable).
- **Transport**: HTTP/1.1 and a single WebSocket.
- **Version**: `v1`, in the path. See [Versioning](#11-versioning-and-compatibility).
- Verified against Herdr 0.9.0, protocol 22.

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
`agent.prompt` are, by design, remote code execution as the logged-in user. The
server binds `127.0.0.1` only and is reachable remotely exclusively through a
tunnel the user started.

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
  "api_version": "v1",
  "server_version": "0.1.0",
  "herdr_version": "0.9.0",
  "herdr_protocol": 22,
  "features": ["binary_frames", "pairing", "expose"],
  "limits": {
    "max_frame_bytes": 65536,
    "min_cols": 20,
    "min_rows": 6
  },
  "ui": { "theme": "auto", "default_view": "grid" }
}
```

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

Pairing exists so a phone never has to type a long token.

```
  phone                         server                     user's laptop
    |                              |                             |
    |                              |<--- herdr-expose pair -------|
    |                              |  mints a 6-char code, 10 min |
    |                              |  prints it and a QR          |
    |                              |                             |
    |--- POST /v1/pair ----------->|                             |
    |    {"code":"7K2QX9", ... }   |  verify, single-use, burn it |
    |<-- 200 {"device_token": ...} |                             |
    |                              |                             |
    |--- GET /v1/stream (Bearer) ->|                             |
```

The QR encodes the full URL so a scan needs no typing at all:

```
https://herdr.example.com/#pair=7K2QX9
```

**`POST /v1/pair`**

```json
{
  "code": "7K2QX9",
  "device_name": "Pixel 8",
  "platform": "android"
}
```

`200 OK`:

```json
{
  "device_id": "dev_01HQ8R2K9M",
  "device_token": "yq8Zr1Wd3pK7sVx0aB4cE6gH9jL2nP5tQ8uY1wA3zC7",
  "expires_at": "2026-10-16T09:12:00Z"
}
```

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
| **Data** | BINARY | fixed 11-byte header plus raw bytes | terminal output and input |

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

- `seq` is a `uint64`, monotonic per connection, assigned by the server. Clients
  may omit `seq` on messages they send.
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

Declare interest in targets. A `target` is a Herdr pane identifier, ASCII, as it
appears in `tree`.

```json
{ "type": "subscribe",   "data": { "targets": ["w1:p1", "w1:p2"] } }
```

```json
{ "type": "unsubscribe", "data": { "targets": ["w1:p2"] } }
```

#### `resize` — required before the first frame

Terminal geometry, **per target, per connection**.

```json
{ "type": "resize", "data": { "target": "w1:p1", "cols": 80, "rows": 24 } }
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
  "w1:p1": "live",
  "w1:p2": "summary",
  "w1:p3": "none"
}}}
```

#### `command`

Call a Herdr socket method. See [section 9](#9-calling-herdr-methods).

```json
{ "type": "command", "data": {
  "id": "c-17",
  "method": "pane.send_text",
  "params": { "pane_id": "w1:p1", "text": "ls" }
}}
```

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
  "api_version": "v1",
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
  "herdr_connected": true,
  "workspaces": [{
    "id": "w1",
    "label": "herdr-expose",
    "cwd": "/Users/me/src/herdr-expose",
    "tabs": [{
      "id": "w1:t1",
      "label": "build",
      "panes": [{
        "id": "w1:p1",
        "terminal_id": "term_8f21",
        "title": "claude",
        "cwd": "/Users/me/src/herdr-expose",
        "agent": "claude",
        "status": "working",
        "idle": false,
        "focused": true,
        "cols": 120,
        "rows": 40,
        "alive": true,
        "mode": "live"
      }]
    }]
  }]
}}
```

Notes that matter for a client:

- **`id` is not stable across Herdr server restarts. `terminal_id` is stable
  across moves.** Key your UI state on `terminal_id` where you can.
- `status` is one of `idle`, `working`, `blocked`, `unknown`. `blocked` means the
  agent is waiting on a human — that is your cue for the Q&A view.
- `idle` is a server fact. **DONE is not.** See `agent` below.
- `mode` is the mode the **server** chose for you. It may not be what you asked
  for in `viewport`.
- `herdr_connected: false` means we lost the Herdr socket and are retrying. Keep
  the client connection open and show a degraded state; a fresh `tree` follows
  reconnection.

#### `agent`

Agent status transitions, ahead of the next full `tree`.

```json
{ "seq": 87, "type": "agent", "data": {
  "target": "w1:p1",
  "status": "blocked",
  "detection": "Do you want to proceed? (y/n)"
}}
```

`detection` is the raw text Herdr classified on. Render it as-is; **do not parse
per agent kind** — that is a maintenance treadmill and it breaks on every agent
update. Pair it with a fixed key bar (y / n / enter / esc / arrows / 1 2 3).

**Deriving DONE.** The server reports `idle`, which is a fact about the pane. It
does **not** report DONE, which is a fact about *you*. Each connection keeps its
own unseen set:

```
DONE = idle AND unseen-by-this-connection
```

Mark a pane seen when the user focuses it, not when you read it. Your "mark all
read" affects only your device — with a laptop and two phones attached, a global
seen set would mean whoever glances first wipes everyone else's badges. Seen
state lives on the client; the server will not keep it for you.

#### `result`

Response to a `command`, correlated by `id`.

```json
{ "seq": 88, "type": "result", "data": {
  "id": "c-17", "ok": true, "result": { "written": 3 }
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
{ "seq": 120, "type": "closed", "data": { "target": "w1:p1", "reason": "exited" } }
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
| `2` | `snapshot` | raw ANSI bytes: a full repaint of the target |
| `3` | `gap` | exactly 8 bytes: `u64` big-endian `bytes_dropped` |

- All integers are **big-endian** (network byte order).
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

`seq` is a `uint64`, monotonic per connection, assigned across **both** planes
from one counter. It is for **ordering and debugging**.

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

---

## 9. Calling Herdr methods

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
| `403` | origin not allowlisted |
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
- **New entries in `features`** on `/v1/config` and `welcome`.
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
// 1. bootstrap
const cfg = await fetch("/v1/config", {
  headers: { Authorization: `Bearer ${token}` }
}).then(r => r.json());

// 2. connect
const ws = new WebSocket(`wss://host/v1/stream?token=${token}`);
ws.binaryType = "arraybuffer";

ws.onopen = () => send({ type: "hello", data: { client: "demo", protocol: 1 } });

ws.onmessage = (ev) => {
  if (typeof ev.data === "string") {
    const msg = JSON.parse(ev.data);
    switch (msg.type) {
      case "welcome": break;
      case "tree":    render(msg.data); break;
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
