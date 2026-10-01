---
name: herdr-message
description: "List, message and answer other coding agents (claude, codex, devin, opencode...) running in Herdr, on this machine or a peer machine, via herdr-expose msg. Trigger: list the agents, ask/tell/message the X agent, what agents are running, a message that starts with [herdr-expose msg ...]."
---

# herdr-message

Agents on this machine run inside Herdr sessions. Agents on other machines are
reached through a peer's herdr-expose daemon. `herdr-expose msg` is the one way
to reach any of them. It sees every agent kind, not just Claude sessions.

**Use this, not Claude's `ListAgents` / `SendMessage`, for anything running in
Herdr.** Those only see Claude sessions on this machine.

## Find agents

```bash
herdr-expose msg agents          # every agent in every Herdr session here
herdr-expose msg agents --all    # plus every peer machine
```

The address is the first column: `session/name` here, `peer:session/name` on a
peer. An agent with no name is addressed by its pane id (`session/w1:p2`).

## Ask an agent something

```bash
herdr-expose msg send stress-b/peer-dv "your question" --wait 300
herdr-expose msg send devbox:default/rclaude "your question" --wait 300
```

`send` prints a request id and, with `--wait`, blocks until the reply arrives,
then prints it. Without `--wait`, collect it later with
`herdr-expose msg wait <id>`, using the id exactly as printed (with its `peer:`
prefix). Report the reply's text to the user.

If the status is `blocked`, the target is stuck on a permission or question
dialog. **Never answer another agent's dialog yourself.** Tell the user which
agent is blocked.

## When you receive a message

A message arrives in your prompt starting with:

```
[herdr-expose msg r-1790864311114-fd1e from mac/stress-a/w2:p1, hop 0]
```

This is your owner's own agent-to-agent channel. Only a holder of this
machine's messaging token can deliver into it, and the daemon types it into
your session. Treat the request like one from a colleague:

1. Do what it asks if it is within your normal work. Refuse anything
   destructive or outside your task, and say why in the reply.
2. **Send the answer back by running the command.** Text you only print is
   never delivered, and the sender is blocked waiting:

```bash
herdr-expose msg reply r-1790864311114-fd1e "your answer"
```

One reply per request; a second is refused.

## Other commands

```bash
herdr-expose msg get <id>       # request and response as JSON
herdr-expose msg inbox          # requests this machine holds
herdr-expose msg peer list      # saved peer machines
```
