# 35. `share --all`: the whole herdr, time-boxed

Status: Accepted (extends SPEC AMENDMENTS 9 §G2 and AMENDMENTS 10; does not
touch the ladder of AMENDMENTS 16 / ADR 0029)

## Context

A share was always pinned to ONE session via `--only`. That left exactly two
ways to let somebody see more than one agent:

1. Create several shares — several processes, several ports, several pairing
   codes, and a lifecycle to track per URL.
2. Leave the **permanent** daemon's tunnel up (`herdr.deemwar.com`) and hand
   over a device pairing. That instance serves everything, is not time-boxed,
   and revoking it means revoking a device on the daily driver.

Option 2 is what people actually do, and it is the worse of the two: the
standing endpoint has no deadline. "Show me everything for an hour" had no
answer, so it was answered with "here is everything, forever".

The core already supported the shape: a nil `*Scope` is the ordinary
unrestricted server (`internal/core/scope.go`). Only the CLI insisted on a pin.

## Decision

`herdr-expose share --all` creates a share with **no scope**: it serves every
running herdr session, exactly as the main daemon does, while keeping
everything that makes a share a share — its own detached process, port, auth
store and log; a mandatory TTL; the three-way expiry; pairing at every rung;
and self-destruct on revoke or expiry.

1. **`--all` is the absence of a scope, never a bypass of one.** It passes no
   `--only`, so `core.ParseScope` returns nil and the store is unrestricted.
   Scope enforcement for scoped shares is untouched — a weaker scope would have
   been a second code path through the filter, and the filter is the thing that
   must not have two code paths.
2. **`scope: "all"` is recorded explicitly** in `share.json` and reported by
   `share list --json`. Empty already means "old record / absent field"
   everywhere else, and "this share serves the whole machine" must never be
   something a reader infers from a missing value. The run path REFUSES a
   record with no session that is not marked `all`.
3. **It is confirmed out loud.** Before a port, directory, token, tunnel or DNS
   record exists, the CLI names the sessions it is about to expose, says that
   this is every session and not one, states the rung and the exact expiry, and
   waits for a typed `yes`. `--yes` pre-answers it for scripts; no terminal and
   no `--yes` is a refusal, not an assumption. A scoped share gets no prompt:
   its blast radius is one session that the person just named.
4. **`--all` and `--session`/`--pane` are mutually exclusive.** The two answers
   differ by the whole machine, so a command line that gives both is a mistake,
   refused at parse time.
5. **The rung axis is unchanged.** `--all` composes with `--local`, `--lan`,
   `--quick` and `--domain` and moves none of them. Scope says how much is
   behind the URL; the rung says how far the URL reaches.

## Consequences

- An all-sessions instance has the DAEMON's command surface, not the narrowed
  `ScopedCommands` allowlist — because it is the daemon's shape. That is the
  real cost of `--all`, and it is why the confirmation exists.
- The honest argument for the feature is expiry: a time-boxed, revocable,
  pairing-gated window over everything is strictly safer than leaving the
  permanent tunnel up to achieve the same thing.
- `list`, `extend`, `pair`, `revoke`, `revoke --all`, `restore`, `panic` and
  the sweeper needed no changes: they operate on the record, and the record
  only gained a value in a field it already had.
