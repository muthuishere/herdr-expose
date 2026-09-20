# Architecture

A single Go binary, installed as a Herdr plugin, that serves a web UI and (later)
mobile apps. Herdr stays upstream and untouched.

---

## 1. Principles

1. **Never reimplement Herdr.** Panes, tabs, workspaces, worktrees, agent
   detection, session restore — all upstream. This binary is transport + UI.
2. **One artifact.** `go build` produces a binary with the React app inside it.
   No node at runtime, no separate deploy, no static file paths to configure.
3. **The socket is the only upstream contract.** Never parse Herdr's TUI, never
   read its state files. If something isn't in the socket API, it doesn't exist.
4. **Clients are dumb.** Server owns layout, state, and output mode selection.
   Web and mobile render what they're told. Two clients showing different things
   is a bug in the server, not the client.
5. **Loopback only.** The binary never binds a public interface. Remote access
   is always a tunnel the user starts.

---

## 2. Topology

```
┌────────────────────────────────────────────┐
│  Herdr server (rust, upstream, untouched)  │
│  panes · agents · worktrees · detection    │
└──────────────────┬─────────────────────────┘
                   │ unix socket / named pipe
                   │ $HERDR_SOCKET_PATH
┌──────────────────▼─────────────────────────┐
│  your-plugin  (one Go binary)              │
│                                            │
│  upstream/   socket client, event fanout   │
│  core/       state store, terminal hub,    │
│              viewport scheduler, seen sets │
│  wire/       protobuf codec                │
│  serve/      WS hub, HTTP, pairing, auth   │
│  webui/      go:embed dist/                │
└──────────────────┬─────────────────────────┘
                   │ 127.0.0.1:7420
        ┌──────────┼───────────┐
        │          │           │
    React web  cloudflared  adapter
                   │           │
              ┌────▼───────────▼────┐
              │  iOS · Android      │
              └─────────────────────┘
```

---

## 3. Repo layout

```
cmd/plugin/main.go          serve | daemon | status | stop | pair
internal/upstream/          herdr socket client
  client.go                 connect, request/response, reconnect
  events.go                 event subscription + fanout
  poll.go                   fallback pollers for anything not evented
  model.go                  types generated from `herdr api schema --json`
internal/core/
  store.go                  authoritative tree; single writer goroutine
  hub.go                    terminal streams, one per active target
  viewport.go               live/summary mode selection
  seen.go                   per-connection unseen sets
internal/wire/              generated protobuf + framing
internal/serve/
  ws.go                     websocket endpoint
  http.go                   healthz, pair, static
  auth.go                   bearer tokens, pairing tokens
web/                        react + vite source
web/dist/                   build output, embedded
herdr-plugin.toml           manifest
```

`internal/upstream/model.go` is **generated**, not hand-written:

```
herdr api schema --json --output schema.json
go run ./tools/schemagen schema.json > internal/upstream/model.go
```

Regenerate on every Herdr bump. A compile error there is the cheapest possible
way to learn upstream changed something.

---

## 4. Plugin manifest and lifecycle

`herdr-plugin.toml` declares a build step, a startup hook, and actions. Build
prefers a local Go toolchain and falls back to a prebuilt release binary, so it
installs with or without Go present (this is what herdr-plus does).

```toml
id = "you.mux"
min_herdr_version = "0.9.0"        # oldest version whose APIs you actually use
platforms = ["linux", "macos"]

[[build]]
command = ["./scripts/build.sh"]

[[startup]]
command = ["./bin/mux", "daemon"]

[[actions]]
id = "open"
command = ["./bin/mux", "open"]     # prints/opens the local URL

[[actions]]
id = "pair"
command = ["./bin/mux", "pair"]     # prints a QR for mobile

[[actions]]
id = "status"
command = ["./bin/mux", "status"]
```

**The lifecycle constraint.** Startup hooks are one-shot initialization
commands, not supervised background services. Herdr runs the hook once after
restore when the API is ready, and will not restart the process if it dies.

Handle it in the binary, not the manifest:

- `mux daemon` — fork-execs `mux serve` detached, writes `$XDG_RUNTIME_DIR/mux.pid`,
  exits 0 immediately so the startup hook completes cleanly.
- `mux serve` — the real server. Takes an exclusive flock on the pidfile and
  exits quietly if another instance holds it. This makes double-start harmless,
  which matters because the hook fires on every Herdr session restore.
- Ship an optional systemd/launchd unit for people who want crash recovery and
  boot persistence. Self-daemonizing is the default because it makes
  `herdr plugin install` produce a working UI with zero extra steps.

---

## 5. Upstream layer

### Connection

Prefer `HERDR_BIN_PATH` for one-shot commands — it keeps you portable across
Unix sockets and Windows named pipes. Use the raw socket for the streaming and
event paths where the process-spawn cost per call would be absurd.

Reconnect with exponential backoff. On reconnect, resync the whole tree rather
than diffing: it's one request and it can't be wrong.

### Events vs polling

Verify against `herdr api schema --json` before building this — the split
determines how much of the layer is push and how much is a poller. Known-good
example: `worktree.created` is a real event (herdr-plus subscribes to it).

Anything without an event gets a poller in `poll.go`, on a slow cadence
(1–2s), and is marked in the store as poll-derived so latency expectations are
visible in the UI rather than mysterious.

### Terminal streams

`terminal session control` and `terminal session observe` are the streaming
primitive: newline-delimited JSON, `terminal.frame` records with base64 ANSI
bytes, `terminal.closed` at teardown. Control accepts `terminal.input`,
`terminal.resize`, `terminal.scroll`, `terminal.release` on stdin.

**Decode base64 once, here.** Everything downstream is binary. Paying that cost
per client, over a tunnel, on cellular, is the single easiest performance
mistake to make.

**One upstream stream per target, regardless of client count.** The hub fans
out. Ten clients watching the same pane is one upstream stream.

**Preserve control exclusivity.** Herdr allows one controller and unlimited
observers, with explicit takeover. Map that straight through — it is what makes
attaching a phone safe while the desktop TUI is open. Dropping it corrupts input
in ways that are miserable to debug.

---

## 6. Core

### Store

Single writer goroutine owning the tree. Everything else reads immutable
snapshots. No mutexes on the hot path, no torn reads, and the ordering of
"tree changed" versus "output arrived" is deterministic.

Sequence numbers are assigned here, monotonic per connection, with a ring buffer
per target sized in bytes (say 256KB) rather than messages. Reconnecting clients
replay from their last seq; if the ring has rolled past it, they get a `Gap`
followed by a `Snapshot`.

### Viewport scheduler

Clients declare what they're rendering. The server picks the mode:

| Mode    | When                        | Cost                          |
|---------|-----------------------------|-------------------------------|
| LIVE    | focused pane                | full stream, ~16ms coalescing |
| SUMMARY | visible but unfocused       | `--source visible` at 1–2 Hz  |
| NONE    | offscreen                   | state changes only            |

Clients never request LIVE directly. A browser tab with twenty live xterm.js
instances will stall; twenty snapshot tiles will not, and the same mechanism
keeps the TUI cheap for unfocused panes.

Coalescing: buffer output per target, flush on a ~16ms tick or at 64KB,
whichever first. Bursty agent output collapses into one frame per paint.

### Seen state

Herdr's `done` is idle-but-unseen; `pane focus` and `agent focus` mark seen,
reads do not, and each TUI client already tracks completions independently.

**Seen must be per-connection here.** With web, TUI and two phones attached, a
global seen set means every client wipes everyone else's Done badges. The server
holds authoritative idle; each connection holds its own unseen set and derives
DONE locally. Focus from a client marks seen for that connection only.

### Backpressure

Per-connection send queue with a hard cap. On overflow: drop buffered output for
the affected target, emit a `Gap` with the byte count, then a `Snapshot`.

Never grow the buffer, never block the hub, never silently drop. Losing output
under load is acceptable. Lying about it is not.

---

## 7. Serve layer

```
GET  /healthz          liveness, unauthenticated
POST /v1/pair          one-time token -> long-lived key
GET  /v1/stream        websocket, binary protobuf frames
GET  /*                embedded SPA
```

One origin, one port. `cloudflared tunnel --url localhost:7420` exposes all of
it with no additional configuration.

### Auth

- Bind `127.0.0.1` only. Never `0.0.0.0`. The tunnel is the sole remote path, so
  a leaked token doesn't also expose you to everyone on the coffee shop wifi.
- Bearer token in the WebSocket handshake. Reject on mismatch before upgrade.
- Pairing: `mux pair` mints a short-lived one-time token and prints it as a QR.
  The device exchanges it at `/v1/pair` for a long-lived key it stores in
  Keychain/Keystore. One-time tokens expire in 60s and are single-use.
- Per-key revocation, listed by `mux status`.

**Take this seriously.** `pane run` and `agent prompt` execute arbitrary
commands. The moment a tunnel is up, this binary is a remote code execution
endpoint. Treat auth as the primary feature it is, not a checkbox.

### Static assets

```go
//go:embed all:web/dist
var webFS embed.FS
```

Serve with correct cache headers: hashed asset filenames get immutable, the
index gets no-cache. Vite emits hashed names by default.

---

## 8. Web UI

React + Vite + TypeScript. Protobuf types generated from the same `.proto` the
server uses, so a client that compiles is a client that agrees.

- **State**: one store fed by the WS stream. No client-side derivation of tree
  structure — render exactly what the server sent.
- **Terminal**: xterm.js only for LIVE panes. SUMMARY tiles render the ANSI
  snapshot into a lightweight canvas or a pre-rendered HTML block; instantiating
  an xterm.js per tile is what kills the tab.
- **Reconnect**: on drop, retry with backoff, resume with last seq. Show a
  degraded-connection indicator using the Ping RTT rather than pretending
  everything is fine.
- **Q&A view**: when an agent goes `blocked`, show
  `agent read --source detection` (the same bottom-buffer region Herdr
  classifies on) plus a fixed key bar — y, n, enter, esc, arrows, 1/2/3. Generic
  across all agent kinds, no per-agent parsing to maintain.

---

## 9. Mobile (later)

Same protocol, no server changes. What's genuinely different:

- **Push.** APNs and FCM need a server reachable from Apple's and Google's
  networks; a laptop behind a tunnel isn't. Either the user runs a small hosted
  relay, or notifications only arrive while the app holds an open connection.
  This is the one piece that can't stay fully self-hosted. Design it now, even
  if you build it later.
- **Trigger**: `agent prompt --wait --until blocked` gives you notification
  events without polling. Caveat: it doesn't track individual turns, so if the
  agent was already working, that turn's completion can satisfy your wait. Treat
  the wait as "something settled," then re-read state.
- **Terminal fallback**: SwiftTerm on iOS is solid. Check the license on any
  Android terminal widget — several good ones are GPL, which is a problem
  alongside proprietary code on Play.

---

## 10. Performance budget

| Path                        | Target                            |
|-----------------------------|-----------------------------------|
| Keystroke to upstream write | < 5ms                             |
| Output to LIVE client (LAN) | < 20ms including coalescing       |
| SUMMARY tile refresh        | 1–2 Hz                            |
| Tree resync on reconnect    | < 100ms                           |
| Idle CPU, 20 panes          | < 1%                              |
| Memory, 20 panes            | < 60MB (ring buffers dominate)    |

Enable per-message deflate on the WebSocket. Terminal output compresses roughly
10x, which is what makes a tunnel usable on cellular.

---

## 11. Failure modes

| Failure                    | Behavior                                        |
|----------------------------|-------------------------------------------------|
| Herdr server stops         | Mark disconnected, hold client connections open, reconnect with backoff, resync tree |
| Herdr upgraded, schema drift | Log loudly, surface `herdr_version` in Welcome, keep serving what still parses |
| Client too slow            | Gap + Snapshot, never unbounded buffering        |
| Two clients want control   | Second gets `CONTROL_HELD`; retry with takeover  |
| Pane dies mid-stream       | `TerminalClosed`, tree update, client shows dead |
| Tunnel drops               | Client reconnects, resumes from seq              |
| Startup hook fires twice   | flock on pidfile, second instance exits 0        |

---

## 12. Build

```bash
# scripts/build.sh
cd web && npm ci && npm run build && cd ..
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o bin/mux ./cmd/plugin
```

`CGO_ENABLED=0` gives a static binary and clean cross-compilation. Release
matrix: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.

Manifest build step tries the local Go toolchain first, falls back to
downloading the matching release asset — so users without Go still get a working
install.

---

## 13. Milestones

1. **Bridge spike.** Connect to the socket, list panes, subscribe one terminal,
   send a keystroke. One afternoon. Proves the entire architecture.
2. **Schema generation.** `herdr api schema` → generated model. Do this before
   writing any hand-rolled types.
3. **Server core.** Store, hub, viewport scheduler, WS endpoint. No UI.
   Exercise it with a CLI test client.
4. **Web UI.** Tree, one live pane, one summary grid. Ship this — it's already
   useful to you daily.
5. **Agent surfaces.** `agent start`, `agent prompt --wait`, blocked Q&A view.
6. **Plugin packaging.** Manifest, build script, daemonization, `plugin install`
   from GitHub end to end.
7. **Tunnel + pairing.** QR flow, token exchange, cloudflared docs.
8. **Mobile.** Only after the protocol has stopped changing.

Steps 1–4 are the real product. Everything after is expansion.

---

## 14. Open questions to resolve first

- Which upstream events are pushed vs. must be polled? → `herdr api schema --json`
- Does the detection snapshot reliably contain the full prompt for each agent
  kind, or does it clip? → `agent explain --verbose --json` across several agents
- Is there a supervised-service story in the plugin manifest beyond one-shot
  startup hooks? → `herdr.dev/docs/plugins` event-hook list
- What is Herdr Cloud's shape when it lands, and does it obsolete your tunnel
  layer or slot in behind your transport interface?
