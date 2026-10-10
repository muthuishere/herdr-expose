# 39. A login password — the weaker, convenient second credential

Status: Accepted

## Context

ADR 0017 gave this binary two secrets — a server token and per-device tokens —
and made pairing the only way a new device gets one. Pairing is strong: the code
is minted by the local CLI and displayed **only on the physically present
machine**, so reaching the public URL is never enough. That property is exactly
what makes `lan` mode (ADR 0022) and shares (ADR 0026) defensible.

It is also a cost. Every new device has to be standing in front of the machine,
or have the code read to it, before it can connect. An operator who simply wants
to open their own exposed app from a second device, and is willing to trade some
of pairing's strength for not having to walk to the laptop, has no option but
pairing.

A password is that option. It is, deliberately, a **weaker** credential: a human
chooses it and a human types it, so it has far less entropy than a 256-bit token
and it can be guessed. On a binary that runs arbitrary commands behind a tunnel,
a weak credential is only acceptable if it is hardened to the same posture as
everything else here.

## Decision

Add an **optional** login password, set by the operator, exchanged by the
exposed web app for an ordinary device token through `POST /v1/login`. It is a
pure addition: pairing, the server token and loopback/LAN behaviour are
unchanged, and the password is off until one is set.

Mechanics, all non-negotiable:

- **argon2id, never sha256hex.** Tokens are high-entropy, so a fast hash is fine
  for them; a password is weak, so it gets a deliberately slow, memory-hard hash
  (`m=64MiB, t=3, p=4`). The only thing between a leaked state file and the
  password is the cost of each guess. Parameters are stored inside the encoded
  hash and read back on verify, so tuning them does not strand an old password.
- **Hashes only, in state (0600), never in `config.toml`** — the same rule as
  every other secret (ADR 0009, ADR 0017). There is no plaintext `password` key.
  Presence of a hash **is** the "enabled" switch.
- **Set by command, never by argument.** `herdr-expose config set-password` reads
  from a no-echo prompt or piped stdin; an argument would land in `ps` and shell
  history. `config clear-password` removes it.
- **A strict, dedicated rate limiter**, separate from the pairing/authfail one
  and far stricter (burst 5, ~0.1/s). It is checked **before** the argon2 verify,
  because an unlimited login route is a CPU-exhaustion DoS, and it is the
  load-bearing brute-force defence. The **global** bucket carries the weight:
  behind a tunnel every remote client shares one source address, so a per-address
  bucket alone is a shared bucket a stranger can empty — the global bucket caps
  total guesses per second no matter how many addresses they come from, exactly
  as the pairing-code limiter does. A per-address bucket is added on top so one
  client cannot eat the whole global allowance.
- **Constant-time compare** of the derived key (`crypto/subtle`), as everywhere.
- **Opaque failure.** Wrong password, no password set, and rate-limited all
  return the same bare `401`. The route never reveals whether a password is even
  configured — a distinguishable answer is an oracle.
- **The minted token is identical to a paired one** — 32 random bytes, hash
  stored, clamped to the instance deadline, LRU-evicted, revocable, expiring with
  a share. A share whose deadline has passed mints nothing.
- **The password is never logged** — only that one was set, cleared, or used.

`/v1/config` exposes a `password_set` **boolean** (never the hash), the same
kind of pre-auth discovery as `auth_required`: the web app cannot know to offer
the password form otherwise.

## Consequences

- A second device gets in without a walk to the laptop — the convenience this
  exists for — at a documented, bounded cost in credential strength.
- That cost is real: a password is guessable where a token is not, so the global
  rate limiter is now a load-bearing control, not hygiene, and is unit-tested
  directly (burst trips, then recovers).
- Pairing stays the strong default and the only credential some operators ever
  set. Leaving the password unset is a fully supported state, and the safest one.
- The embedded web app still needs a password field wired to `/v1/login`; the
  backend credential is complete without it.
