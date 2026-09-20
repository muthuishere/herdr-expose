# Contributing to herdr-expose

herdr-expose puts live Herdr terminal sessions on a phone. That means it runs on
somebody's real machine, spawns real processes, opens a real hostname to the
internet, and streams bytes that are frequently secrets. The rules below exist
because of that, not because of taste.

## Build

```sh
./scripts/build.sh              # web bundle, then the Go binary with it embedded
./scripts/build.sh --source     # fail instead of falling back to a release asset
./scripts/build.sh --skip-web   # Go only; web/dist must already exist
```

The binary is a single file with the React app inside it (ADR 0001). There is no
separate web server to run.

## Gates — run these before you open a PR

```sh
go build ./... && go vet ./...
go test -race ./...             # the gate. -race is not optional.
./e2e/acceptance.sh             # the real one: builds, stands up its OWN
                                # throwaway herdr session, exposes it on the LAN,
                                # drives a browser through pairing, tears it down
```

`acceptance.sh` never touches a session, share or daemon it did not create — the
session name is forced to start with `hexe2e-` and every herdr call is pinned to
that session's socket. Keep it that way. `HEX_HEADED=1` to watch it.

If you changed anything on the fanout or streaming path, also run the stress
harness in `e2e/stress/` and put the numbers in the PR.

## House rules

**Measure before you claim.** A performance number that nobody watched a process
reach does not go in the spec, the README or a PR description. SPEC A3 and
AMENDMENTS 20 are a table of numbers somebody measured; add to it the same way.
"A claimed number is not a number."

**Never log pane bytes, and never log a secret's value.** Terminal output is the
user's API keys, their `.env`, their customer data. It goes to the client and
nowhere else — not to the log at any level, not into an error string, not into a
test fixture. Credentials are verified by USING them and reporting the outcome
(see `doctor`'s Cloudflare check); the value never reaches stdout, a log line or
a state file. `redactSecrets` is the belt, not the trousers.

**The exposure ladder is never climbed for you.** Local, LAN, quick tunnel and
named domain are four separate, explicit requests (AMENDMENTS 16/17). A failure
at one rung degrades LOUDLY; it never quietly widens exposure to succeed. If your
change makes something "just work" by reaching further onto the network, it is
wrong even if it is convenient.

**Looking must not touch.** Read paths are read-only. A viewer must never resize
a PTY, send input, or change anything about a pane its owner is working in
(AMENDMENTS 14). Transcript is the default view for every pane for this reason.

**A share always expires.** Every share is time-boxed, scoped in the SERVER
rather than the UI, and self-destructing — three independent expiry enforcements,
because one is not enough (AMENDMENTS 9/10). No flag, no config key and no code
path may produce a share that outlives its deadline.

**Diagnostics must not contradict reality.** `doctor` is read-only and its job is
to be believed. A check that can print FAIL for a working system is worse than no
check — it sends people chasing a problem that is not there.

## Where the decisions live

- `SPEC.md` — the spec, and AMENDMENTS 1-20 which supersede it in order. The
  amendments are authoritative; read the marker at the top of a section before
  trusting its body.
- `docs/adr/` — 35 ADRs, one decision each, with the reasoning and what was
  rejected. `docs/adr/README.md` indexes them.
- `docs/api.md` — the client contract. A Swift or Kotlin client must be buildable
  from it alone, with no access to this source (A4). If you change the wire, you
  change that file in the same commit.

A change that contradicts an ADR or an amendment is not automatically wrong — but
it needs a new ADR or amendment saying so, in the same PR. Silently building on
top of a spec that no longer describes the code is how the next person inherits a
fiction.

## Pull requests

Small, one concern, with the gate you ran named in the description. See
`.github/PULL_REQUEST_TEMPLATE.md`.

## Reporting a security issue

Do not open a public issue. Email muthuishere@gmail.com.
