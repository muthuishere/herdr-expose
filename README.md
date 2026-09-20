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
