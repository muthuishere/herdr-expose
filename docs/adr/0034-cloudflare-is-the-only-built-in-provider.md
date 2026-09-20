# 34. Cloudflare is the only built-in provider; everything else is an adapter

Status: Accepted (SPEC AMENDMENTS 18; **narrows** ADR 0028's provider parity and
removes the `ngrok = true` clause of ADR 0005)

## Context

herdr-expose shipped two built-in providers. ADR 0005 gave ngrok the same
static-domain rule as Cloudflare, and ADR 0028 went further and made provider
parity a *property*: `quick` was defined as "the ephemeral rung of whichever
provider is selected", so `--quick --provider ngrok` and `--quick` landed on the
same rung with the same guarantees — verify before publish, supervision,
symmetric teardown, secrets by env-var name.

That parity was real in the code and it was **never real in the world**.

There is no `ngrok` binary on the machine this was built on, and no ngrok
account or authtoken. Every ngrok test was therefore written against a fake
binary that echoes a hostname and sleeps: they proved the *shape* of the
integration — the right argv, the token in the child's environment and nowhere
else, the right scanner regex — and they could not prove that ngrok's agent
actually behaves that way. The provider was never once started for real, never
had a URL of its own answer a request, never had its process die and come back.

**Code on a public surface that has never actually run is a liability.** It
rots quietly against the upstream tool's flags and log format, and the first
person to use it is the one who finds the bug. The owner's call was shorter:
"no need ngrok — cloudflare is enough."

## Decision

**Delete the built-in ngrok provider.** With it goes the `ngrok` config key,
`--provider`, the ngrok rungs, `$NGROK_AUTHTOKEN` handling, and every claim in
the README, the skill, the docs and the config scaffold that herdr-expose
carries ngrok.

**The `Provider` interface and `Footprint` stay.** They are not ngrok
scaffolding. `Footprint` is what makes teardown symmetric with creation *when
creation was interrupted* — the case that orphans resources — and the interface
is what lets idempotency be tested without a network. cloudflare-named,
cloudflare-quick and the goja adapter all implement it, and the rung × provider
table in `internal/expose/provider_test.go` still runs over all three. The
abstraction does not collapse back into Cloudflare-specific code, and the JS
adapter is IN that table rather than beside it.

**The goja/JS adapter is the answer for everything else**, and it is promoted
from curiosity to extension point. `adapters/template.js` documents the whole
host API and what an adapter owes the host; `adapters/ngrok.js` stays as a
worked example, clearly labelled as community-shaped code that CI parses and
never runs.

**`--provider` is removed rather than reduced.** With one built-in it would be a
one-value flag pretending to be a choice. It is refused at parse time with an
error that names the replacement, because silently accepting `--provider ngrok`
and handing back a Cloudflare tunnel is the one outcome worse than failing.

**A config that still carries `ngrok = true` keeps loading.** It is read without
complaint, ignored, and dropped on the next rewrite — exactly as the withdrawn
`server.bind` (ADR 0022) and `default_mode = "auto"` (ADR 0029) keys are. A key
that no longer does anything is not a reason to take somebody's daemon down.

## Consequences

- **The surface shrinks to what is exercised.** Every remaining transport in the
  Go binary is one that runs on the machines this is developed and deployed on.
  That is the property worth keeping, and it is the criterion for admitting a
  future built-in: not "is it popular" but "can we run it end to end here".
- **ngrok users are not abandoned, they are relocated** — to a ~40 line adapter
  that lives next to the binary they already have installed and authenticated,
  and that they can actually verify. That is a better home for it than a Go file
  nobody here can run.
- **The escape hatch now carries real weight**, so it has to be documented as
  such rather than as a footnote, and it has to be held to the built-in's
  standard in tests. Both are done; if the adapter path regresses, the removal
  becomes a real loss rather than a tidy-up.
- **A share recorded under the old provider is still reapable.** `share list`
  and `share revoke --all` read it, classify it as creating nothing in anybody's
  Cloudflare account, and wipe it. Restoring one is refused by name instead of
  being quietly re-pointed at Cloudflare, which would aim a tunnel at a hostname
  in somebody else's account.
- **We lose the second implementation that kept the interface honest.** A single
  implementation is how an interface drifts into being one class's public
  methods. The JS adapter in the provider table is the mitigation, and it is a
  weaker one than a second built-in was — noted here rather than glossed over.

## Alternatives considered

**Keep it and mark it experimental.** A label is not a test. The failure mode is
unchanged: the first real user finds the bug, and now they find it in something
we shipped with a disclaimer.

**Keep it and verify it in CI.** This needs an ngrok account, a reserved domain
and a token in CI — a paid dependency and a long-lived credential on a public
surface, to support a transport the owner does not use. Out of proportion.

**Reduce `--provider` to one value.** Rejected: a flag that accepts exactly one
argument teaches the wrong thing about what the tool can do, and keeps a switch
alive for a branch that no longer exists.
