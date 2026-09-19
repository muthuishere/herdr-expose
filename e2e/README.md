# The acceptance test

`wsprobe` proves the *protocol* answers. This proves the **product works**: a
real Chromium, in front of a real `herdr-expose share --lan`, in front of a real
Herdr session running a real coding agent. Every assertion is something a human
would notice going wrong.

```bash
./e2e/acceptance.sh
```

That is the whole thing. It builds, sets up, asserts, and tears down.

## What it actually does

1. Builds `web/dist` and the Go binary into a temp dir.
2. Starts its **own** throwaway Herdr session (`hexe2e-<n>`), with three panes:
   an agent pane (`claude`), and two plain shells.
3. `share --lan --hours 1` on that session only, and mints a pairing code.
4. Drives Chromium through the pairing screen, the `?pair=` deep link, the pane
   tree, the terminal, typing, the blocked-agent panel, a 65-second idle soak
   and a 375px viewport.
5. Revokes the share, stops the session, deletes it, and sweeps for orphans.

Teardown runs from a shell trap, so it happens on failure and on Ctrl-C too.

## The assertions

| # | What it proves | How it is checked |
|---|---|---|
| 1 | An unpaired device gets nowhere | `.pair-input` is on screen |
| 2 | `?pair=<code>` pairs a device, phone-style | the tree renders; the code is scrubbed from the URL |
| 3 | The tree is readable | the session name, human pane titles, and **no** `w1:t1`-style ids as labels |
| 4 | The terminal shows live agent output | the browser is watching when `herdr agent prompt` fires; the reply token must appear, cross-checked with `herdr pane read` |
| 5 | Typing in the browser reaches the pane | keystrokes are typed into xterm, then read back with `herdr pane read` |
| 6 | The badge is the truth | badge `data-state` must agree with `herdr agent get`, and must go to `working` while the agent works |
| 7 | A blocked agent gets a question and answer keys | `herdr pane report-agent … --state blocked`, then the pane must be pinned under "Needs you", the badge must flip, `.qa` must show the question, and the y/n/1/2/3 bar must be there |
| 8 | **An idle pane still streams** | subscribe, sit for `HEX_IDLE_SECS` (65 by default), then make the pane print a token and require it in the browser |
| 9 | No flicker | two screenshots 2s apart, pixel-diffed in-page (< 2%), plus `__herdrStats()` showing `reset` and `termCreate` unchanged at 1 across the idle window |
| 10 | 375px is a first-class layout | the mobile shell renders, a pane opens, the terminal draws, nothing overflows |

Check 8 is the important one. It is the failure a stress test found and the
reason this file exists: a LIVE target that dies quietly a few seconds after
attach looks identical to a pane that simply has nothing to say.

## Knobs

| Variable | Default | What |
|---|---|---|
| `HEX_HEADED=1` | off | watch the browser |
| `HEX_KEEP=1` | off | leave the session and share up afterwards |
| `HEX_IDLE_SECS` | `65` | length of the idle soak |
| `HEX_BIN` | built fresh | test an existing binary instead of building |
| `HEX_SESSION` | `hexe2e-<n>` | must start with `hexe2e-`, or the script refuses |
| `HEX_AGENT_KIND` | `claude` | any kind `herdr agent start` supports |
| `HEX_ARTIFACTS` | `e2e/artifacts` | screenshots, `stats.json`, `results.json` |

Comparing two builds is the useful trick:

```bash
git archive HEAD | tar -x -C /tmp/head && cp -R web/dist /tmp/head/web/dist
( cd /tmp/head && go build -o /tmp/hex-head ./cmd/herdr-expose )
HEX_BIN=/tmp/hex-head HEX_ARTIFACTS=/tmp/art-head ./e2e/acceptance.sh
```

## Two things worth knowing

**The DOM renderer is forced.** WebGL and Canvas draw the terminal into a
`<canvas>`, which no test can read back as text. The harness seeds
`herdr-expose.renderer-blocklist` before boot — the app's own shipped fallback
path — so `.xterm-rows` carries real text.

**Panes are opened by id, never by title or row position.** Row order changes
the moment something is pinned under "Needs you", and a pane's title is whatever
its shell last set. `openPane()` clicks candidates until `__herdrStats().viewport`
says the intended target is `live`.

## Safety

It only ever touches what it created:

- the session name is forced to start with `hexe2e-`;
- every `herdr` call is pinned to that session's socket by environment;
- the share is `--lan`, scoped with `--session`, and expires in an hour;
- teardown revokes the share, stops only its own server, and reports (does not
  kill) anything left holding the session name;
- afterwards it checks that the daemon on `:21118` still answers `/healthz`.

It never stops a Herdr server it did not start, never restarts the daemon, and
never goes near the permanent tunnel.
