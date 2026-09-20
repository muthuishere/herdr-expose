# herdr-expose

**Reach your terminal agents from anywhere on your network — and hand someone
else a link when they need to work on one.**

Your agents run on one machine. You are not always at that machine, and
sometimes the person who should answer an agent's question isn't you.

herdr-expose puts a [Herdr](https://herdr.dev) session in a browser — Claude
Code, Codex, Devin, Gemini, Copilot, Cursor and the other 15 agent kinds Herdr
detects, or any plain shell — reachable
from your other laptop, a tablet, your phone, or a client's machine. They read what the agent is
doing, answer it, drive it — through a link scoped to one session that expires
on its own.

One Go binary. The web app is inside it.

---

## Quick start

```bash
herdr plugin install muthuishere/herdr-expose   # binary + plugin + agent skill
herdr-expose share                              # a URL for this session
```

That's it. The second command prints a link and a pairing code. Open it on any
device on your network — or send it to whoever needs to work on that agent.

Or skip the sharing and just open it locally: `herdr-expose daemon` then
[127.0.0.1:21118](http://127.0.0.1:21118).

---

## What you can do

| | |
|---|---|
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
| [**Decisions**](docs/adr/) | 36 ADRs — what was chosen, and what it cost |
| [**Contributing**](CONTRIBUTING.md) | build it, run the gates, house rules |

Requires Herdr 0.9.0+. macOS and Linux. MIT — see [LICENSE](LICENSE).

**Windows isn't supported yet.** Herdr itself runs there; this doesn't, because
it dials a Unix socket where Windows uses a named pipe, and it relies on `flock`
and process groups that have no Windows equivalent. That's real work rather than
a build flag — [open an issue](https://github.com/muthuishere/herdr-expose/issues)
if you want it.
