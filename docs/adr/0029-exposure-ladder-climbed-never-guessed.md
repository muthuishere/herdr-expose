# 29. The exposure ladder is climbed, never guessed

Status: Accepted (SPEC AMENDMENTS 16 and 17; withdraws the auto-escalation
introduced with ADR 0028, and supersedes ADR 0022's automatic promotion for
shares)

## Context

Once `--quick` existed (ADR 0028), a bare `herdr-expose share` resolved
`domain -> quick -> lan`: use `[expose] domain` if it looks usable, else a quick
tunnel if `cloudflared` is installed, else the LAN.

That is convenient and it is wrong. It means the tool can put an agent session
on the **public internet** because a config key happened to be set, months ago,
for something else entirely. The person typed four words and got a URL; nothing
in what they typed said "the internet".

This binary runs arbitrary commands inside the owner's agent sessions. The blast
radius of each rung differs by orders of magnitude — this machine, this room,
the entire internet — and the step from "this room" to "the entire internet"
must never be a config file somebody forgot they set.

## Decision

Four rungs. **Every rung is an explicit request**, and the default is the least
exposure that still does the job.

| rung | flag | reach |
|---|---|---|
| **lan** — the DEFAULT for a share | none | `http://<lan-ip>:<port>` — this machine and this network |
| cloudflare | `--quick` | `https://<random>.trycloudflare.com` |
| custom domain | `--domain X` | `https://X` |
| local | `--local` | `http://127.0.0.1:<port>` — an explicit opt-IN, for testing |

The rungs are modelled as an **ORDER** (`config.Rung`), not as a chain of
if-statements, so the invariant is a comparison the code can make and a test can
assert: *the resolved mode is never above the requested mode.* Every resolution
path funnels through one guard that checks it. A future edit reintroducing an
auto-escalation fails there, loudly, instead of quietly publishing a session.

**Fallback only ever goes DOWN, and it says why.** An explicit `--quick` or
`--domain` with no `cloudflared` resolvable degrades to LAN with a printed
reason — the person *did* ask to be reachable, and a LAN address is the nearest
honest answer. Nothing moves the other way: a `--lan` request never becomes a
tunnel, and a bare `share` never becomes anything but LAN.

**The share default is `lan`; the daemon default is `local`.** Same principle,
different job. A share exists to be reached from somewhere else — somebody
sitting at the machine would just open `localhost:21118` — so a loopback-only
share is the one rung that makes the feature pointless. The daemon serves the
person at the machine, so loopback is right for it. Binding `0.0.0.0` covers
loopback and the network in one, so the LAN default costs the local user
nothing.

`[share] default_mode` exists for a different personal default and ships as
`lan`. It is deliberately the **only** config key that can move a bare `share`
off the LAN, and an unrecognised or empty value (including the withdrawn
`"auto"`) reads as `lan` — the failure mode of a misread key is never a tunnel.

## Consequences

- Typing `--quick` costs a second, and it makes reach a conscious choice. That
  is the only thing that makes the pairing, scope and TTL guarantees elsewhere
  in this design mean anything.
- `[expose] domain` being set no longer influences a bare `share` at all. That
  is a behaviour change from ADR 0028's ladder and it is the point of this ADR.
- Callers must report the rung they **got**, not the one they assume:
  `.share.mode` in the JSON. Note that a `--domain` share reports
  `mode: "cloudflare"` (the resolved transport), not `"domain"` (the rung name).
- The top-level `herdr-expose --help` still describes the withdrawn auto ladder.
  `herdr-expose share --help` is correct. That is a known documentation defect
  in the CLI, not a second behaviour.
