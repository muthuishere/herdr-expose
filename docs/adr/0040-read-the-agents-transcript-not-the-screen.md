# 40. Read the agent's own transcript, not the screen it painted

Status: Accepted. **Supersedes** ADR 0030's premise for AGENT panes only — a
transcript view is still right, but it is no longer built from the pane. 0031
("looking must not touch") is untouched and is strengthened: this path reads a
file and cannot resize anything. The pane remains the source for a plain shell
and for what is happening right now.

## Context

ADR 0030 decided agent panes get a readable transcript rather than a terminal
mirror, and built it from `pane.read`. That was right, and it has a ceiling
that cannot be raised by better rendering: **the information is gone before we
see it.**

Claude Code renders markdown to ANSI before it paints. A heading becomes SGR
bold. A fenced block becomes nothing — the fence characters are not drawn at
all. Then both read paths ask for `strip_ansi: true`, which removes even the
bold. The measurement was already in the tree, in `web/src/components/inlineText.tsx`:

> across three live Claude Code panes: one `**bold**`, zero headings, zero
> fences, zero inline backticks.

So the transcript view was doing heuristic archaeology on a rendered grid:
`detectHardColumn`, `unwrapHardCuts`, `seam`, `isContinuation`, a punctuation-density
code classifier. That machinery exists to undo a 120-column hard wrap, and it
is where the "words torn in half" and "the question vanished" bugs came from.
It was correct work on the wrong input.

Meanwhile the agent writes the real thing to disk continuously.

## Decision

**For an agent pane, render the agent's own structured transcript. Keep the
pane as the fallback and as the live view.**

1. **The source.** Claude Code's `~/.claude/projects/<slug>/<sessionId>.jsonl`:
   markdown source intact, plus `tool_use`/`tool_result` as discrete objects.
   Discrete is the point — a tool-call boundary inferred from rendered text is
   a boundary that will be wrong, and collapsing needs the boundary.

2. **The mapping is given to us, not guessed.** Herdr's `agent.list` already
   carries `agent_session: {source: "herdr:claude", kind: "id", value: "<uuid>"}`
   and that uuid IS the Claude session id. Verified: 7 of 7 live agents
   resolved. The obvious alternative — match on the working directory — is
   wrong in practice: of 32 live agents on this machine there are 27 distinct
   cwds, so five share a directory and cwd-matching would show them each
   other's conversations. The id also means the web client needs no new
   addressing: it already has it as `pane.agent.id`.

3. **The id is a name, not authority.** `/v1/agentlog` resolves the id back to
   the pane that owns it and applies the same scope gate as the rest of the
   API. Without that, a share pinned to one session could read any session
   whose id it learned, which is strictly more than the share was allowed to
   show. `targetForAgentSession` is a pure function of (tree, scope, id) so
   that this rule is testable without an HTTP handler and a live Herdr.

4. **A separate endpoint, not another stream field.** The stream is the fanout
   path and ADR 0014's budget is about exactly that path. A shared poller
   pushing tens of KB of markdown and tool output to every subscriber on every
   tick would spend the streaming budget on history nobody is reading. This is
   fetched when somebody opens a pane.

5. **Markdown renders to React nodes, never to HTML.** No
   `dangerouslySetInnerHTML`, so there is no sanitiser to get wrong — the XSS
   surface is absent rather than mitigated. That also rules out every markdown
   library in this space, since they all render to an HTML string and hand the
   sanitising problem back. The renderer is ~9.6KB of the bundle (310 → 320
   KiB); `marked` + `DOMPurify` is six times that and brings the surface with
   it.

## Consequences

**What a reader gets.** The thing they came for: headings, fences, tables,
lists and inline code as themselves, and 63% of the view given back. That
figure is measured, not estimated — 826 tool_use/tool_result pairs against 487
prose blocks in one real session. Collapsed to one row each (`▸ Bash  go test
./...`) the prose becomes the document again.

**The hard-wrap repair becomes unnecessary** on this path. Prose arrives as one
unwrapped line per paragraph, so the browser reflows it at any width. The
existing shaper stays for shell panes, where there is still a grid to repair.

**Three caps, each from a measurement.** A tool result is capped at 4KB and
says when it was cut (largest measured: 431,524 bytes — at that size the
question is not how to display it but whether to send it). The reader seeks a
512KB tail and discards the partial first line (transcripts reach 33MB; half a
JSON object is otherwise a parse error forever on exactly the files big enough
to need the seek). A request is capped at 400 events.

**What a share exposes grows, and this was the owner's call.** The structured
transcript carries more than the visible scrollback: full tool outputs and
files the agent read, within those caps. The alternative considered and
rejected was redacting tool bodies, which would have kept the exposure
identical and removed most of the value — a collapsed row you cannot open is a
row that says only that something happened.

**Dropped deliberately**, each for a stated reason: sidechains (a subagent's
conversation, which the parent's `Agent` call already stands for),
`<system-reminder>` blocks and slash-command plumbing on user turns (injected
context shown verbatim makes the transcript look like the person said things
they never said), and empty thinking blocks — 464 measured, every one
signature-only, and a collapsed section that is always empty teaches people to
ignore collapsed sections.

**It is per-machine and Claude-only, and the Devin gap is named rather than
filled.** This reads a file, so it works for agents on the machine
herdr-expose runs on. Devin's richer store is `sessions.db`, a SQLite file —
its JSON transcripts carry `tool_calls` but hold only 100 of 190 sessions and
the busiest one is absent, and the db is the only place with
`working_directory`. There are 31 live claude agents on this machine and zero
devin. Writing a Devin reader that no live agent can exercise is the mistake
ADR 0034 was written about, so the seam exists and the implementation waits for
a Devin agent to test it against.

**The pane is still authoritative for the present moment.** The live spinner
and the permission prompt are not in the transcript, so a blocked agent is
still read from the screen, and the view swap is one tap.
