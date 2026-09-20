# 27. Ephemeral DNS is legal — for shares only

Status: Accepted (SPEC AMENDMENTS 9 §G4; the single exception to ADR 0005)

## Context

ADR 0005 is categorical: the tunnel is static, `cloudflare = true` requires a
`domain`, and `expose stop` never removes DNS. That rule exists so a
**permanent** endpoint keeps a stable hostname — PWA installs, bookmarks and
origin-bound device tokens all break when a hostname moves.

A share is the opposite kind of object. It is disposable by construction, and
none of those objections bind to something that will not exist tomorrow.

## Decision

A `--domain` share is the **one** place this codebase creates and then deletes
DNS. At expiry it must `destroy`: stop `cloudflared`, delete the DNS record
**only if it is tagged `herdr-expose-share`**, delete the tunnel, and wipe the
share state directory including its tokens.

Leaving a dead hostname resolving to a tunnel that no longer exists is worse
than having no record at all — it is a name that looks alive and answers 530.

Two guards keep this from touching the permanent deployment:

- **The comment tag is the ownership marker.** A share may only ever delete a
  record it created, identified by that tag. A record we did not create is never
  touched, in either direction.
- **A share may not take the permanent deployment's hostname.** That collision
  is a hard error naming the fix, not a silent downgrade, because the failure
  mode would be hijacking the daemon's own URL and then deleting it an hour
  later.

Zone access is checked **before** anything is created, so a share never
half-provisions.

## Consequences

- Two teardown paths exist, and the destructive one is only reachable from the
  share code path. That asymmetry is deliberate and is asserted in tests.
- `--quick` and `--lan` shares create nothing in Cloudflare at all, so their
  teardown reports `cloudflare_not_applicable: true` alongside `dns_gone` and
  `tunnel_gone` — meaning "there was never one", not "we deleted it". A caller
  must not read those booleans as evidence of a deletion.
- A share needs a zone the API token can reach, which is why `--domain` is the
  only rung with real preconditions and why `--quick` (ADR 0028) exists.
