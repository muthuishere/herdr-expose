# 26. A share is a scoped, time-boxed, self-destructing second instance

Status: Accepted (SPEC AMENDMENTS 9 and 10)

## Context

The permanent deployment (ADR 0005) answers "let me reach my machine from my
phone." It does not answer the other half of what this tool is for: *"let
somebody else look at this one agent session, for an hour."*

Doing that on the main daemon would mean scoping a live, long-running process
per client, which puts the whole machine one authorisation bug away from a
guest. And the obvious shortcut — hand out a device token for the permanent URL
and tell them not to click around — is not a security model, it is a request.

## Decision

**A share is a separate process.** `herdr-expose share` spawns a detached
`herdr-expose` on its own free port, with its own state directory under
`<state>/shares/<id>/`, its own auth store, its own log file, and `--only
<session>` set. One share = one process = one port = one scope = one set of
credentials. The daemon on 21118 is untouched and keeps serving everything.
Killing a share cannot affect the daemon or another share.

**Scope is enforced in the store, not the UI.** A scoped instance's tree
*contains* only what is shared. Everything else is not hidden client-side — it
is absent from the tree, absent from `subscribe`, and refused at the hub if a
target outside scope is requested. The command surface narrows too: prompt,
send-keys, read, scroll and resize on in-scope targets only; no `pane.run`, no
session enumeration, no plugin methods. A scoped share must never be one client
bug away from exposing the rest of the machine.

**Every share is time-based. There is no permanent mode.** A long-lived share
is a long TTL (`--days 30`), not a different code path. That is the whole point:
one code path means there is no branch where a share outlives its token, escapes
the sweep, or skips teardown. Expiry is enforced three ways, because one is not
enough:

1. an in-process timer that destroys the tunnel and DNS and exits;
2. every issued device token carries `expires_at <= ` the share's expiry, so a
   token cannot outlive the share even if teardown fails;
3. `share.json` records the deadline, and both `share list` and the main
   daemon's periodic sweep reap shares whose deadline passed and whose process
   is gone — crash-safe cleanup.

**Restore across a reboot** respawns a live share with its **original**
deadline, never a refreshed one; a share whose deadline already passed is reaped
instead. `share extend` is an explicit, logged act that pushes the deadline on
`share.json` **and** on the already-issued device tokens, or the tokens expire
underneath a live share.

**Pairing still applies.** A share is not open. It mints a pairing code at
start, printed locally alongside the URL and the QR — same rules as everywhere
(ADR 0017), plus the share-scoped expiry.

## Consequences

- One more process per share, and a supervision story for each. Accepted: the
  isolation is the feature, and a share that crashes takes nothing with it.
- Teardown is the risky part, so it **reports per component** —
  `process_gone`, `dns_gone`, `tunnel_gone`, `port_free`, `state_wiped` — and
  `revoke` / `panic` exit non-zero if anything survived. A kill switch that
  silently half-works is worse than none.
- Cloudflare refuses to delete a tunnel until its edge connections are marked
  inactive, roughly a minute after `cloudflared` dies. So teardown deliberately
  **keeps `share.json`** (credentials wiped immediately, state `orphaned`) and
  lets the next sweep finish the job. Deleting the paperwork would strand the
  tunnel with nothing left to clean it up.
- `share.json` on disk is the source of truth for a share's life, which means
  the reaper is testable without a network.
