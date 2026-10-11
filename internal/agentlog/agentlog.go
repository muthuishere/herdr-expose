// Package agentlog reads an agent's OWN structured transcript, instead of
// scraping the terminal it happens to be painting into.
//
// # WHY THIS EXISTS
//
// The pane view is built from `pane.read`, which gives us the rendered screen
// with ANSI stripped. That is the right source for a shell and the wrong one
// for an agent, and the reason is not a matter of taste — it is that the
// information is already gone by the time we see it. Claude Code renders
// markdown to ANSI before it paints: a heading becomes SGR bold, a fence
// becomes nothing at all, and `strip_ansi: true` then removes even the bold.
// The measurement is in web/src/components/inlineText.tsx and it is brutal:
// across three live panes, ONE surviving `**bold**`, zero headings, zero
// fences, zero inline backticks.
//
// Meanwhile the agent has been writing the real thing to disk the whole time.
// Claude Code keeps a JSONL transcript per session with the markdown source
// intact, plus tool calls and their results as DISCRETE OBJECTS — which is the
// only way a UI can collapse them, because a boundary you have to guess at
// from rendered text is a boundary you will get wrong.
//
// Three facts measured on this machine's real transcripts, each of which
// decided something here:
//
//   - 63% of blocks are tool noise (826 tool_use/tool_result pairs against 487
//     text blocks). Collapsing is the feature, not a nicety.
//   - One tool_result was 431,524 bytes. So results are CAPPED on the way out
//     (see MaxToolResultBytes); a collapsed section that still ships 431KB to
//     a phone is a bug with a bow on it.
//   - Transcripts reach 33MB. So this reads the TAIL and never the file (see
//     tailLines); parsing 33MB on a poll loop is not a thing we are going to do.
//
// # HOW A PANE FINDS ITS TRANSCRIPT
//
// It does not have to guess. Herdr's `agent.list` already carries
// `agent_session: {source: "herdr:claude", kind: "id", value: "<uuid>"}`, and
// that uuid IS the Claude session id. Verified against all 7 agents in the
// live session: 7 resolved, 0 missed. That matters because the obvious
// alternative — matching on the working directory — is ambiguous in practice:
// of 32 live agents on this machine, 27 distinct cwds, so five agents share a
// directory with another and cwd-matching would have shown them each other's
// conversations.
//
// # WHAT THIS IS NOT
//
// It is not a second source of truth about what the agent is doing. The pane
// remains authoritative for "what is on screen right now", including the
// agent's own live spinner and its permission prompts, none of which are in
// the transcript. This is the readable history; the pane is the present
// moment.
package agentlog

import (
	"strings"
	"time"
)

// Role is who produced an event. Deliberately a small closed set: the UI
// renders each one differently and an unrecognised role has no rendering.
type Role string

const (
	RoleUser      Role = "user"      // what the person asked
	RoleAssistant Role = "assistant" // prose, in MARKDOWN SOURCE
	RoleTool      Role = "tool"      // a tool call and its result, as one unit
)

// Event is one thing that happened, flattened to what a transcript view needs.
//
// A tool call and its result are ONE event rather than two, because that is
// how they are read: "it ran this, and this came back". Keeping them apart
// would make the client re-pair them by id, which is work it does not need to
// do and a chance to get wrong.
type Event struct {
	Role Role `json:"role"`

	// Text is markdown SOURCE for the assistant and the user. It is not
	// pre-rendered and must not be: rendering is the client's job, and it is
	// the only side that knows how wide the screen is.
	Text string `json:"text,omitempty"`

	// Tool is the tool's name ("Bash", "Read"). Empty unless Role is RoleTool.
	Tool string `json:"tool,omitempty"`

	// Summary is the one line a COLLAPSED tool event shows: the command for a
	// Bash call, the path for a file read. It is the whole reason a collapsed
	// list is useful rather than 800 rows of the word "Bash".
	Summary string `json:"summary,omitempty"`

	// Result is what the tool returned, capped at MaxToolResultBytes.
	Result string `json:"result,omitempty"`

	// ResultTruncated says the cap bit. Surfaced rather than swallowed: a
	// reader who cannot tell a short result from a truncated one will debug
	// the wrong thing.
	ResultTruncated bool `json:"result_truncated,omitempty"`

	// Failed is the tool's own is_error, not our inference from its text.
	Failed bool `json:"failed,omitempty"`

	At time.Time `json:"at,omitempty"`
}

// MaxToolResultBytes caps one tool result on the way to a client.
//
// 4KB, which is about 50 lines — enough that the common case (a test summary, a
// short file, an error) arrives whole, and far below the point where a phone
// notices. The largest result measured on this machine was 431,524 bytes; at
// that size the question is not how to display it but whether to send it, and
// the answer is no.
const MaxToolResultBytes = 4096

// MaxSummaryChars bounds the one-line collapsed summary. A `Bash` command can
// be a 2,000-character pipeline, and a summary that wraps to nine lines is not
// a summary.
const MaxSummaryChars = 160

// capResult applies MaxToolResultBytes, cutting on a RUNE boundary so the
// result is still valid UTF-8, and on a LINE boundary where one is near the
// cut, because ending mid-line reads as corruption rather than as a cut.
func capResult(s string) (string, bool) {
	if len(s) <= MaxToolResultBytes {
		return s, false
	}
	cut := MaxToolResultBytes
	// Back off to a rune boundary.
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	out := s[:cut]
	// Prefer the last newline, but only if it is not throwing away most of
	// what we kept.
	if nl := strings.LastIndexByte(out, '\n'); nl > MaxToolResultBytes/2 {
		out = out[:nl]
	}
	return out, true
}

// utf8Start reports whether b can begin a UTF-8 sequence (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// oneLine flattens text to a single line for a collapsed summary: a summary
// that contains a newline breaks the one-row-per-call layout that makes a
// collapsed list readable.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	// Runes, not bytes: cutting a multi-byte character in half here would put
	// a replacement glyph in the middle of a label.
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}
