# Security

## Reporting a vulnerability

Use GitHub's **private vulnerability reporting** on this repository
(Security → Report a vulnerability). That gets it to me privately, with a
place to coordinate a fix and a release.

Please do not open a public issue for a vulnerability first.

There is no bounty. I will credit you unless you ask me not to.

## What this tool is, so you can judge severity

herdr-expose gives a browser control of a Herdr terminal session. Typing into
that terminal runs commands as the user who started the server. **Remote code
execution is the feature**, not a bug class to be eliminated — a terminal you
cannot type into is a screenshot.

So the security properties worth attacking are the ones that decide *who* gets
to type:

| property | where it is enforced |
|---|---|
| A device token is required in every mode except a loopback listener | `internal/serve/auth.go` |
| Auth is skipped only because the LISTENER is loopback — never because a header says so | `newLocalMode`, and `bypassAuth` also requires a loopback peer, a pinned Host and no proxy/CDN headers |
| Origin allowlist and Host pinning, enforced in every mode | these are what defeat DNS rebinding against a local listener |
| A pairing code is displayed only on the machine itself; **no endpoint mints one** | `POST /v1/pair` only redeems |
| Tokens are stored as SHA-256 hashes, 0600, never in the config file | `internal/serve/auth.go` |
| A share's scope is enforced in the store, not the UI — an out-of-scope session is absent from the tree and refused at the hub | `internal/core/scope.go` |
| A share always expires; there is no permanent share | deadline, token `expires_at`, and a reaping sweep |
| Secrets are read from the environment by name at point of use and never logged, stored or returned | `internal/expose/redact.go` |

A bug that lets someone **type into a session they were not granted**, obtain a
pairing code remotely, read a session outside a share's scope, outlive a
share's expiry, or extract a token or API key from a log, status output or an
error message is a vulnerability. Please report those.

## Things that are known and deliberate

- Anyone on the network can reach a `--lan` share's port. They still cannot pair
  without seeing the screen. This is stated at create time.
- A `--lan` share is plain HTTP, so it is not a secure context: no service
  worker, no PWA install. Only a tunnel gives those.
- A `--quick` share is on the public internet on a guessable-length random
  hostname. It is pairing-gated, scoped and time-boxed, but it is public.
- `/healthz` is unauthenticated by design. It returns only `{"ok","api"}` to an
  anonymous caller; the detailed body requires auth.
