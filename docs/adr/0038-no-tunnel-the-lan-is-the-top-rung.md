# 38. herdr-expose does not raise tunnels; the LAN is the top rung

Status: Accepted. **Supersedes** 0005, 0027, 0028 and 0034 entirely, **removes**
0022's `cloudflare` mode, and **truncates** 0029's ladder from four rungs to two.
0004's loopback default, 0023's local-mode pinning, 0026's scoped time-boxed
share and 0035's `--all` are untouched and now carry the whole product.

## Context

Five ADRs in this directory are about one thing: how herdr-expose puts a pane
on the public internet. 0005 chose a static Cloudflare domain provisioned
through the API from Go. 0027 carved out ephemeral DNS for shares. 0028 brought
quick tunnels back for shares. 0034 cut the second provider after discovering
its tests had only ever run against a fake binary. 0029 built a ladder so that
none of it could happen by accident.

That is a lot of decision for a feature the owner can get from one command of a
tool that already exists on this machine:

```
cftunnel add 21118 herdr
```

What herdr-expose was carrying to do the same job:

- a Cloudflare API client — zones, DNS records, tunnel create/delete, token
  validation;
- credential minting, so `cloudflared login` and its browser prompt were never
  needed;
- a cloudflared subprocess supervisor with restart, a halt-flag protocol and a
  managed-pid ledger;
- DNS reconciliation, an upsert that had to adopt its own orphaned records, and
  teardown that re-read the API to verify what it had deleted;
- a goja JavaScript host, so that any transport Cloudflare was not could be
  written as an adapter;
- two extra rungs on the ladder, each with its own fallback rule, its own
  teardown shape and its own set of invariants to hold.

About 6,300 lines, and the most dangerous code in the product. Every bug in it
had the same blast radius: a pane on the public internet.

The deciding argument is not line count. It is that **this is the wrong binary
to put it in**. herdr-expose reads and writes the owner's agent sessions — it
runs arbitrary commands inside them. A tunnel that lives in the same process
means every mistake in the tunnel code can reach the sessions, and every
mistake in the session code can be reached from the internet. A separate tool
that forwards a TCP port cannot accidentally forward a pane.

## Decision

**herdr-expose serves loopback and the local network, and nothing else.**

1. The ladder is two rungs: `local` (127.0.0.1) and `lan` (0.0.0.0). There is
   no rung above `lan`, for the daemon or for a share.
2. Reaching the server from outside the network is the owner's own business: a
   tunnel they run, a VPN, an SSH forward. The product points at `cftunnel` by
   name because that is what is installed here, not because it is special.
3. **Nothing fails silently.** `--quick`, `--domain` and `--provider` are
   REFUSED with where they went, never downgraded to a LAN share. A config file
   still carrying `cloudflare`, `domain`, `tunnel_name` or `adapter` still
   LOADS — an upgrade that refuses to start is the one failure nobody can work
   around — but the keys are named at startup and dropped on the next rewrite.
   `[share] default_mode = "quick"` reads as `lan` and says so.
4. A share RECORD on a withdrawn rung is refused by the instance rather than
   downgraded, because `share restore` runs on every daemon start and a public
   share silently returning as a LAN one is a different share wearing its name.
5. The JS adapter host goes with the transports it existed to carry. Chat
   adapters are a different table (`[[chat.adapters]]`) and a different
   contract, and are untouched.

## Consequences

**What gets better.** The failure mode of every bug in exposure becomes
"unreachable" instead of "reachable by strangers". There are no provider
credentials left to hold, so `config.toml` and the state dir have nothing a
leak could expose. There is no subprocess to supervise, no DNS to reconcile and
no teardown to verify — a share's teardown is now three facts (process gone,
port free, state wiped), each of which is checked rather than assumed. The
daemon's own `panic` verb got more honest in the process: it had been writing a
halt flag that nothing polls since the supervisor left, reporting success for
doing nothing.

**What gets worse, stated plainly.** Nothing off this network reaches the
server by itself any more. Somebody who used `herdr-expose share --quick` to
send a link to a colleague now runs two commands instead of one. A reader of
the old docs will type `--quick` and get an error; that error has to carry the
replacement, which is why point 3 above is a decision and not an
implementation detail.

**What this does NOT change.** Auth is still mandatory on every rung. A pairing
code is still shown only on the physically-present machine, and no HTTP
endpoint mints or displays one — which is the only reason `lan = true` was ever
acceptable, and is now the only reason a tunnel somebody else points at the
port is acceptable either. Shares are still scoped, still time-boxed and still
revocable. The exposure ladder keeps its ORDER rather than collapsing into a
bool, so the "resolved never exceeds requested" check still has something to
compare, and would still hold if a rung were ever added back.

**On the hostname.** `herdr.deemwar.com` kept working across this change
without an outage, because `cftunnel` was already serving it from its own
launchd job, independent of the daemon. That was verified before the code was
removed, not assumed after.
