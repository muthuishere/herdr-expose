# Spec: connecting agents through Herdr

Status: draft, 2026-10-01. Spec first; nothing here is built or run yet.

## Goal

One agent can find another agent and exchange messages with it, at three
distances:

1. **Within a session**: two panes in the same Herdr session.
2. **Across sessions**: two named Herdr sessions on the same Mac (for example
   `openjevx` → `deemwar-one-os`).
3. **Across machines**: this Mac ↔ our dev server `deemwar-dev`, daemon to daemon over HTTP. How the URL is reachable (LAN, tunnel, domain) is an option; no SSH (SSH was used only to set the box up).

Three agent kinds, nothing else: **claude, devin, opencode**.

## How we test: like a person, not a script

Every test starts from nothing and is driven the way a user would drive it:

1. Open a fresh Ghostty window (ghostty-sendkeys).
2. Type `herdr --session <test-session>` into it, so Herdr actually starts in
   that terminal.
3. Create workspaces and panes, and start the agents, through the `herdr` CLI
   typed into the window.
4. Give the first agent its task in **plain words** typed into its pane, for
   example "ask the opencode agent in the other session what directory it is
   in". The test never calls `herdr agent prompt` on the agent's behalf; the
   agent has to work out how to reach the other agent.
5. Check the result from outside (the oracle below), then close everything the
   test created.

We aren't load-testing. Each scenario is a handful of prompts, and the whole
matrix is a few dozen.

## Oracle: what counts as "connected"

The receiving agent's screen is not proof. Every message carries a token, and
the receiver must run `echo <token> >> $RUN_DIR/acks/<receiver>.log`. The sender
must then repeat the receiver's answer back. A pass needs both:

- **Delivered**: the token is in the receiver's ack file.
- **Round trip**: the sender's final reply contains something only the
  receiver knew (its working directory plus the token).

## Scenarios

`S` = sender, `R` = receiver. Each row runs for the listed pairs.

### 1. Within a session

| ID | Scenario | Pairs | Pass |
|---|---|---|---|
| W1 | `S` lists the agents in its own session | claude, devin, opencode as `S` | Lists every agent, with name, kind and status |
| W2 | `S` sends one message to `R`, and `R` answers it | claude→opencode, opencode→devin, devin→claude | Delivered + round trip |
| W3 | `S` starts a new agent of a given kind, uses it, then closes it | each kind starts each other kind | Agent created, used, pane gone afterwards, nothing left running |
| W4 | `R` is busy (mid-task) when the message arrives | claude→opencode | Delivered after `R` finishes, not lost and not mixed into its current task |
| W5 | `R` is blocked on a dialog (trust or permission prompt) | claude→devin | `S` reports that `R` is blocked and doesn't type into the dialog |

### 2. Across sessions (same Mac)

| ID | Scenario | Pairs | Pass |
|---|---|---|---|
| X1 | `S` lists agents in **all** running sessions | claude | Every running session's agents, labelled by session |
| X2 | `S` in session A messages `R` in session B | claude→opencode, opencode→devin | Delivered + round trip |
| X3 | Same agent name exists in both sessions (for example `reviewer`) | claude | `S` reaches the one it was told to, never the other |
| X4 | `S` starts an agent in a different session, uses it, closes it | claude | As W3, in the other session |

### 3. Across machines (this Mac ↔ `deemwar-dev`)

| ID | Scenario | Pairs | Pass |
|---|---|---|---|
| M1 | `S` lists agents on the other machine | claude | That machine's agents, labelled by machine |
| M2 | `S` on the Mac messages `R` on `deemwar-dev` | claude→devin, claude→claude | Delivered + round trip |
| M3 | Reply direction: `R` on `deemwar-dev` messages back to the Mac | devin→claude | Delivered + round trip |
| M4 | The other machine is unreachable | claude | `S` reports it as unreachable within 30 s and doesn't hang |

### 4. Conversation behaviour

| ID | Scenario | Pass |
|---|---|---|
| C1 | Back-and-forth: 5 rounds `S`↔`R` | All 5 delivered, in order |
| C2 | Loop guard: two agents told to keep replying to each other | Stops at the hop limit; doesn't run forever |
| C3 | Chain A→B→C, and C's answer returns to A | A reports C's answer |

## Two runs per scenario

Every scenario runs twice:

1. **Baseline**: Herdr 0.9.3 with only its official skill. This measures what
   works today, with no help from us.
2. **With our layer**: the same scenario with the messaging CLI, envelope and
   skill we build.

The difference between the two runs is the case for building it.

## Requirements before a run

1. Herdr 0.9.3, with the claude, devin and opencode integrations current
   (`herdr integration status`).
2. Tests run in their own named sessions (`stress-a`, `stress-b`), never in
   `openjevx` or `deemwar-one-os`.
3. Every test working directory is under one run directory, and that directory
   is trusted for each kind in advance, except in W5, where an untrusted
   directory is the point.
4. Cross-machine only: our dev server `deemwar-dev` (Linux x86_64, root), saved
   on this Mac as a Herdr machine (`herdr machine add deemwar-dev`, done
   2026-10-01, status reachable). It has herdr 0.9.3, claude 2.1.286 and devin
   with current integrations, both logged in. **No opencode**, so the cross-machine
   pairs use claude and devin. Never a prod box.
   Tests run as the non-root user `agent` (Herdr machine `deemwar-dev-agent`):
   claude in `bypassPermissions`, devin with `DEVIN_PERMISSION_MODE=dangerous`,
   working directory `~/work`. Root's setup is left as it was.

Verified 2026-10-01: from this Mac, `herdr --machine deemwar-dev` created a
workspace, started claude, got a round trip (`HOST=dev-server DIR=/tmp
TOKEN=k7x2`, 8 s), and closed it. Two findings for the spec:

- Claude blocked on its folder-trust prompt at startup on the new box, the same
  failure seen with devin locally. Startup prompts are the first thing our layer
  must handle.
- `--machine` only works **outward**: the Mac drives the dev server over SSH.
  The dev server can't SSH back into a Mac behind home NAT, so M3 (reply back to
  the Mac) can't use `--machine`. It needs a path the Mac exposes, which is
  what the herdr-expose endpoints are for.

## What a run records

For each scenario × pair × run (baseline or with our layer):

- pass or fail;
- which mechanism `S` actually used (Herdr CLI, Claude's `SendMessage`,
  something else, or nothing);
- Herdr error codes seen;
- time from task typed to round trip complete;
- whether anything was left running afterwards.

Results go to `$RUN_DIR/results.jsonl`, plus a one-page summary table.

## Out of scope

Other agent kinds, load and burst testing, the web UI, and Windows.

## Open questions

1. Should the receiver's reply land back in the sender's pane as a new prompt,
   or wait in an inbox until the sender asks for it?

## Baseline results: run 1 (2026-10-01, official Herdr only)

Sender: claude in session `stress-a`, given plain-words tasks that never say
"Herdr". Receivers: opencode `peer-oc` (`stress-a`), devin `peer-dv` (`stress-b`),
claude `peer-remote` (`deemwar-dev-agent`).

| ID | Task | Result | Time | What the sender did |
|---|---|---|---|---|
| W1 | List the agents alongside you | **Fail** | 11 s | Used Claude's `ListAgents`: 26 Claude sessions on the Mac, none of the 3 test peers, no opencode or devin |
| W2 | Message opencode in the same session | **Pass** (ack + cwd) | 54 s | `ListAgents` and `af list` failed first, then it found Herdr and used `herdr agent prompt` |
| X2 | Message devin in another session | **Pass** (ack + cwd) | 30 s | `herdr --session stress-b agent prompt`. devin stopped on a command approval, and the sender **approved it itself** |
| M2 | Message claude on `deemwar-dev-agent` | **Fail** (no ack) | 46 s | Used plain `ssh`, concluded "no herdr on that machine" (`~/.local/bin` isn't on PATH for non-interactive SSH), never tried `herdr --machine` |

What the run shows:

1. **Discovery is the weakest step.** Claude reaches for `ListAgents` first,
   which only sees Claude sessions, and our listing (W1) never reached Herdr.
2. **Same machine works, but slowly and by trial and error**: 30–54 s for one
   message, mostly spent trying the wrong tools first.
3. **Across machines fails.** The official skill never mentions `--machine`.
4. **Safety gap:** a sender approved the receiver's permission prompt on its
   own. Our layer must decide whether that is ever allowed.
5. **Reply back to the Mac (M3)** isn't possible over `--machine`. It needs
   herdr-expose endpoints.

## Design under test: herdr-expose as the messaging hub

Each machine runs one `herdr-expose daemon`. It already talks to every Herdr
session on its box and already has token auth. It gains an agent and messaging
API, and the CLI and skill are thin clients of it.

**Transport is the user's choice, not part of the design.** The daemon serves
plain HTTP endpoints; a peer is just a URL plus a token. How that URL is
reachable is an option: LAN address, `--quick` tunnel, `--domain`, or anything
else that forwards HTTP. Messaging never depends on SSH or on a particular
tunnel. `herdr --machine` (SSH, outward-only) is not used.

```
agent ──CLI/skill──▶ local daemon ──HTTP (any reachable URL)──▶ remote daemon ──herdr agent prompt──▶ agent
                         ▲                                        │
                         └──────────── reply (same path back) ◀───┘
```

### Endpoints (new, `/v1/agents*` and `/v1/messages*`)

| Method + path | Does |
|---|---|
| `GET /v1/agents` | Every agent in every running session on this machine, plus peers' agents with `?peers=1`. Address = `machine/session/name` |
| `POST /v1/agents` | Spawn `{kind, cwd, session, name}`: create pane, wait for shell, start, clear known startup dialogs in trusted dirs |
| `DELETE /v1/agents/{addr}` | Close an agent the API spawned (refuses others) |
| `POST /v1/messages` | `{to, body, reply_to?, hops}` → queued, then delivered with an envelope when the target is idle |
| `GET /v1/messages?to=me&after=<id>` | Inbox, for polling |
| `GET /v1/messages/stream` | SSE push of new messages and status changes |
| `GET /v1/messages/{id}` | Request file plus response file, once one exists |
| `POST /v1/messages/{id}/reply` | `{body}` → writes the response file |

### Rules

- **Envelope** on every delivery:
  `[msg <id> from <machine/session/name> hop <n>] <body> — reply: herdr-expose msg reply <id> "<text>"`
- **Never answers another agent's permission dialog.** A blocked target leaves the
  message `blocked` and the sender is told. (Baseline X2 showed senders will
  approve on their own.)
- **Hop limit** 8 by default, so two agents can't loop forever.
- **Auth:** a new token scope `msg` that is separate from view-only share links.
  A link for watching a pane can't inject messages. Peer daemons pair once and
  store each other's URL + token (`herdr-expose peer add <url>`).
  Every peer request also passes the existing pairing gate; there is no
  unauthenticated path, whatever the transport.
- **Store:** plain files, one pair per request (see below). No database.

### Request id, request and response files, sweep

Every send gets a request id (`r-<unix-ms>-<4 random hex>`, sortable by time).
The daemon that owns the **target** agent keeps the files, in its state dir:

```
<state>/messages/
  r-1759312345678-a3f9.request.json   {id, from, to, body, hops, created, status, error?}
  r-1759312345678-a3f9.response.json  {id, from, body, created}   ← only once replied
```

- `msg send` returns the id immediately. `status` in the request file moves
  `queued → delivered → replied`, or ends in `blocked` / `failed` with the Herdr
  error code.
- The receiver answers with `herdr-expose msg reply <id> "<text>"`, which writes
  the response file **atomically** (temp file + rename), so a reader never sees half a
  reply. Across machines, the reply goes to the target's daemon over HTTP and is
  written there.
- The sender collects it with `msg wait <id>` (`GET /v1/messages/<id>` returns
  the request and, when present, the response).
- A response file is the proof of a round trip. No screen scraping.

**Sweep:** after each write, if there are more than 100 requests, delete the
oldest pairs until 100 remain. Never delete a request that is still `queued` or
`delivered` (waiting for a reply); those stay until they're answered or reach a
24-hour expiry, which marks them `expired`. The limit is configurable
(`messages.keep = 100`).

### CLI and skill

`herdr-expose agents [list|spawn|kill]` and `herdr-expose msg [send|reply|inbox|wait]`
call the local daemon. The skill (`herdr-message`) triggers on plain words ("list
the agents", "ask the devin agent…", "spin up opencode to…") and tells Claude to
use it, **not** `ListAgents` or `SendMessage`, for anything running in Herdr.

### Re-run

Same four baseline tests, plus M3 (`deemwar-dev-agent` → Mac) and C1–C3. Target:
W1 and M2 pass, every message under 10 s, zero self-approved dialogs.

## Run 2 (2026-10-01): with herdr-expose messaging + the herdr-message skill

Test daemons on separate ports (Mac 21200, dev server 21250); the owner's live
herdr-expose was not touched. Dev server reached through a `trycloudflare.com`
quick link.

**Direct sends** (`msg send … --wait`, a reply file proves the round trip):

| Target | Result | Time |
|---|---|---|
| opencode, same Mac | Pass | 5 s |
| devin, other session, same Mac | Correctly `blocked`: local Devin isn't in full-access mode and stopped on its own permission prompt; nothing answered it | — |
| claude on dev server | Pass (8 s); in a later run the reply was written but the quick link was down when fetching it | 8 s |
| devin on dev server | Pass | 8 s |

**Agent-driven** (Claude sender, plain words, skill loaded):

| ID | Baseline (run 1) | Run 2 |
|---|---|---|
| W1 list agents | Fail (Claude-only `ListAgents`) | **Pass, 16 s**: loaded the skill, listed all 28 agents of every kind, flagged the two blocked ones, reported devbox unreachable |
| W2 message opencode | Pass, 54 s | **Pass, 11 s** ("391") |
| X2 message devin, other session | Pass, but **self-approved** devin's prompt | **Correct**: reported devin blocked, did not answer its prompt, gave the request id to collect later |

Fixed during the run:

- **Receivers wouldn't reply.** Claude treated the bare envelope as pasted text
  of unknown origin and asked before replying; Devin printed the reply command
  instead of running it. Fixed with an envelope that says what the channel is
  and that printed text is never delivered, plus the skill on every machine.
- **Wrong binary.** The envelope now names the daemon's own absolute binary
  path, so a reply never depends on the receiver's PATH.
- **Blocked after delivery was invisible** (status stayed `delivered`). The
  daemon now follows delivered requests: `blocked` while the target is on a
  dialog, and never types the message twice.

Open:

- **Quick links are not reliable enough to test with.** The dev server's link
  dropped three times in 20 minutes (QUIC timeout, then edge 530 while the
  connector reported healthy). Messaging itself never failed; the requests and
  replies sat on the dev server waiting.
- The reply's `from` shows a pane id (`w8:p1`), not the agent name.
- Receivers need permission to run `herdr-expose msg reply`. Full-access agents
  have it; an approval-mode agent (local Devin) blocks on it.

## Run 3 (2026-10-01): across machines over `devbox.deemwar.com`

Transport: a named Cloudflare tunnel for the dev server, created once by
`herdr-expose expose start` (with `CLOUDFLARE_ACCOUNT_ID` pinned to the
deemwar.com account). The token was used only for that run and passed over
stdin; the running `cloudflared` holds only the tunnel's credentials file (its
environment was checked: no token), and the daemon config is back to
`cloudflare = false`, so a daemon restart never needs the token.

| Test | Result | Time |
|---|---|---|
| Direct: claude on devbox, arithmetic | Pass ("391") | 5 s |
| Direct: claude on devbox, runs `uname -sm` | Pass | 5 s |
| Direct: devin on devbox, arithmetic | Pass | 4 s |
| Direct: devin on devbox, runs `uname -sm` | Pass | 7 s |
| M1: Mac Claude lists devbox agents (plain words) | **Pass** (baseline: fail) | 13 s |
| M2: Mac Claude asks devbox devin to run `uname -a` | **Pass** (baseline: fail) | 13 s |
| M2b: Mac Claude asks devbox claude for free disk | **Pass** | 11 s |

Not yet run: **M3** (an agent on devbox starts a conversation with an agent on
the Mac). It needs the Mac's messaging endpoint reachable from devbox. The
Mac's live daemon already has `herdr.deemwar.com`, but it runs the released
build without messaging.
