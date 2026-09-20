# 28. Quick tunnels come back — for shares only

Status: Accepted (SPEC AMENDMENTS 15; partially reverses ADR 0005's blanket
ban). Its **provider parity** clause is narrowed by
[0034](0034-cloudflare-is-the-only-built-in-provider.md): there is one built-in
provider now, so parity is a property of the ladder rather than a claim about
two implementations.

## Context

ADR 0005 deleted the quick-tunnel path entirely: no `*.trycloudflare.com`, not
even as a fallback. The reasoning was that a hostname which changes on restart
breaks PWA installs, bookmarks and origin-bound device tokens.

That reasoning is airtight for an endpoint you return to daily, and it does not
apply at all to something that expires in an hour. Applying it as a blanket rule
had a real cost: a share required a domain in the owner's own Cloudflare account,
which meant **nobody else could run this feature at all**.

## Decision

`herdr-expose share --quick` uses an ephemeral TryCloudflare tunnel. No account,
no zone, no DNS, no API token, ~3 seconds to set up, automatic HTTPS and edge
DDoS mitigation, and the tunnel dies with the process.

Everything else about a share is **identical**: scope enforced server-side,
pairing required, time-boxed with the same three-way expiry, and
`share list/extend/pair/revoke/restore` all behave the same.

Teardown is *simpler*, not weaker: no DNS record and no named tunnel exist, so
expiry stops the process and wipes the state. There is nothing to delete in
anybody's Cloudflare account.

**The permanent deployment is unchanged.** `[expose] cloudflare = true` still
REQUIRES `domain` and can never become ephemeral. `quick` is not a key any
config file can set — it is a share flag and nothing else. ADR 0005 stands
exactly where it was aimed.

**`quick` is a RUNG, not a Cloudflare spelling.** It means "the ephemeral
tunnel of whichever provider is selected": a throwaway public hostname with
nothing reserved, nothing provisioned in an account and nothing to clean up. So
the only precondition is whether the selected provider's binary is installed,
and a fallback message names *that* binary rather than always naming
`cloudflared`.

> **NARROWED by [ADR 0034](0034-cloudflare-is-the-only-built-in-provider.md).**
> This clause was written when a second built-in provider (ngrok) carried the
> same two rungs, and it illustrated the rung with "TryCloudflare and an
> unreserved ngrok tunnel are the same rung". That provider was removed as
> **unverified surface**: no ngrok binary and no token on the machine it was
> written on, so it was unit-tested and never once run end to end. The
> definition above is kept rather than deleted, because it is the reason
> `quickBinary` still resolves the binary per provider instead of hard-coding
> `cloudflared` — a JS adapter carries the rung today, and a future built-in
> would inherit the same treatment.

## Consequences

- **A quick share's hostname is new every time**, and device tokens are
  origin-bound, so a phone paired to a previous quick share must pair again.
  The CLI says this once, at create, rather than letting the user discover it by
  re-pairing. Fine for a throwaway; unacceptable for a daily driver — which is
  precisely why the two are different things.
- **A quick URL is not a credential.** It is on the public internet and is no
  more trusted than a domain share: same pairing, same scope, same expiry. An
  unguessable hostname is obscurity, and this binary runs arbitrary commands.
- No Add-to-Home-Screen for a quick share: the install would be pinned to a
  hostname that stops existing when the share expires.
- `revoke --all` and `panic` must handle a mix of `lan`, `quick` and `domain`
  shares in one pass, and one transport's failure must not abort the others.
