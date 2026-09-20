# herdr-expose

**Tell your coding agent "share this" — it hands back a link to itself.**

Your agents run on one machine. You are not always at that machine, and
sometimes the person who should answer an agent's question isn't you.

**The agent skill is the point.** You don't learn a CLI and you don't leave the
session you're in: you say *share this*, and the agent turns that session into a
private URL — open it on your phone, or send it to whoever should answer it.
Everything below is what the agent is driving on your behalf, available to you
too when you'd rather type it.

What lands in the browser is a [Herdr](https://herdr.dev) session — Claude
Code, Codex, Devin, Gemini, Copilot, Cursor and the other 15 agent kinds Herdr
detects, or any plain shell — reachable from your other laptop, a tablet, your
phone, or a client's machine. They read what the agent is doing, answer it,
drive it — through a link scoped to one session that expires on its own.

One Go binary. The web app is inside it. One command installs all three: the
binary, the Herdr plugin, and the skill.

---

## Quick start

```bash
herdr plugin install muthuishere/herdr-expose   # binary + plugin + agent skill
```

That one line installs all three: the binary, the Herdr plugin, and the agent
skill — the skill is linked into `~/.claude/skills/herdr-share` (and
`~/.agents/skills`) by the plugin's build step, so there is no second install
to remember. It is best-effort by design: a skill that won't link is not a
reason to lose the plugin, so if it warns, the plugin is still fine and
`herdr-expose skill install` repairs the skill on its own.

Then, in any agent session on that machine, just say it:

> **share this**

The agent runs `herdr-expose share` for you and reports back the link and the
pairing code. Open it on any device on your network — or send it to whoever
needs to work on that agent. Same thing by hand, if you prefer:

```bash
herdr-expose share                              # a URL for this session
```

Or skip the sharing and just open it locally: `herdr-expose daemon` then
[127.0.0.1:21118](http://127.0.0.1:21118).

---

## What you can do

| | |
|---|---|
| **Just ask** | Say *share this* in the session — the bundled `herdr-share` skill turns your words into the right command and hands back the link |
| **Read an agent** | Agent panes render as readable text that reflows to your screen — not a shrunken copy of a terminal grid |
| **Answer it** | Type a reply, or tap y/n when it's blocked on a question |
| **Give someone a link** | `herdr-expose share` — one session, pairing-gated, gone when it expires. Not SSH to your laptop forever |
| **Reach your own machines** | `herdr-expose share --all` — every session on this box, from any device on the network |
| **Reach it from anywhere** | `--quick` for a throwaway `trycloudflare.com` URL, `--domain yours.com` for your own |
| **Stop everything** | `herdr-expose panic` |

**Looking at a pane doesn't touch it.** Opening one on your phone can't resize
or repaint it on your laptop — which is the mistake most terminal-sharing tools
make, and the thing this was rebuilt around.

---

## Why the skill is the whole point

Every other way to do this is something **you** operate. You install the app,
you scan the QR, you keep the relay account alive, you remember the flag. The
agent is the subject of the sentence and never the one saying it.

`herdr plugin install` puts a skill called `herdr-share` next to the binary,
and after that **sharing is something the agent knows how to do**. Say *share
this*, or *put this on my phone*, or *send Priya a link to this one* — it picks
the verb, runs it, and reads the URL and pairing code back to you. You never
left the session you were in.

Worth saying plainly, because you can check it: Herdr's plugin format has no
concept of a skill at all — the manifest covers actions, panes, keybindings,
link handlers, startup hooks and storage, and the word "skill" does not appear
in its plugin documentation. This plugin installs one anyway, as part of its
build. That is why it feels different to use, and it is the one thing no other
plugin in that marketplace ships.

---

## Next to the alternatives

The row that matters is the first one.

| | **herdr-expose** | VibeTunnel | Omnara | Happy Coder | Claude Remote Control |
|---|---|---|---|---|---|
| **Who starts the share** | **The agent — you say "share this"** | You do | You do | You do | You do — scan a QR |
| **Ships an agent skill** | **Yes — `herdr-share`** | No — terminal proxy, no skill or MCP | No | No | n/a |
| **Account or relay** | **None — your own network, direct** | None | Relay + account, $9/mo; plaintext on their servers unless self-hosted | Free relay, E2E encrypted, self-hostable | Through Anthropic's servers |
| **On a name you own** | **`--domain yours.com`** — your Cloudflare; expiry deletes the DNS record and tunnel it made | Bring your own tunnel, by hand | Their hostname | Their hostname | No |
| **Windows** | **Yes** — verified on real hardware, no Administrator rights | "Windows is not yet supported" | not claimed | Yes | Through WSL |
| **Agents it covers** | 21 kinds Herdr detects, plus any plain shell | Any terminal | Claude Code | Claude Code and Codex | Claude Code only |
| **What a link exposes** | **One session** — pairing-gated, expires on its own | Server-wide auth; no per-session expiry documented | Your account's sessions | Your account's sessions | The session you paired |
| **What it costs you** | It needs Herdr | — | $9/mo | — | A Pro or Max plan |

Two rows are deliberately not a clean sweep. **Happy Coder runs on Windows
too** — VibeTunnel is the one that says it does not — and **herdr-expose is the
only one here that needs another tool installed**, namely Herdr, which the rest
do not. A comparison you can catch out on one row is worth nothing on the
others, so those stay in. Everything above is from each project's own
documentation as of September 2026; check it before you believe it.

---

## Before you run it

This gives a browser control of a terminal. Typing into it runs commands as you.
**That's the feature** — a terminal you can't type into is a screenshot — so the
security is about *who gets to type*:

- Nothing is exposed unless you ask. The default is your own network, and it
  never escalates to the internet on its own.
- Every share needs pairing, and **the pairing code is only ever shown on this
  machine**. No URL can mint one.
- Every share expires. There is no permanent share.

Run `herdr-expose doctor` if anything looks wrong — it checks the lot and tells
you which part is unhappy.

---

## More

| | |
|---|---|
| [**Full guide**](docs/guide.md) | install, exposure modes, config, security model, troubleshooting |
| [**API**](docs/api.md) | the WebSocket + HTTP contract, enough to build a native client |
| [**Agent skill**](skill/SKILL.md) | say "share this session" to Claude Code and it does the rest |
| [**Decisions**](docs/adr/) | 37 ADRs — what was chosen, and what it cost |
| [**Contributing**](CONTRIBUTING.md) | build it, run the gates, house rules |

Requires Herdr 0.9.0+. macOS, Linux and Windows. MIT — see [LICENSE](LICENSE).

**Windows works, and needs no Administrator rights** — verified end to end on
real hardware: the named-pipe transport, file locking, process-tree kill,
`doctor`, `serve`, the web UI, a LAN share, and an agent using the skill to
share its own session. One difference worth knowing before you rely on it:
macOS and Linux install a supervisor that restarts the server if it crashes,
and Windows installs nothing, because a desktop has someone sitting at it.
[docs/windows.md](docs/windows.md) is the check-by-check claim — what was run,
and what was not.
