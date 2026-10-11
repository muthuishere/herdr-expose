package agentlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tailBytes is how much of the END of a transcript is read.
//
// 512KB, against transcripts measured up to 33MB. It is a byte budget rather
// than a line budget because the file is read by seeking, and a seek needs an
// offset: we cannot know where the last N lines begin without reading
// backwards, and reading backwards to save 400KB is complexity bought for
// nothing. At the measured median event size this holds several hundred
// events, which is far more than any view asks for.
const tailBytes int64 = 512 << 10

// maxLineBytes refuses a single absurd JSONL line rather than allocating for
// it. One line is one message, and a message does not legitimately reach 8MB —
// but a tool result CAN (431KB measured, and nothing promises that is the
// ceiling), so the limit is generous and only guards against a pathological or
// corrupt file.
const maxLineBytes = 8 << 20

// ErrNoTranscript means no transcript file exists for that session id. It is a
// NORMAL condition, not a failure: an agent that has not spoken yet has no
// file, a non-Claude agent has none in this format, and a session whose
// history has been cleared has none any more. Every caller must be able to
// fall back to the pane rather than show an error.
var ErrNoTranscript = errors.New("no transcript for that session")

// Reader finds and reads Claude Code transcripts.
//
// Root is the Claude config directory (the one holding `projects/`). Empty
// means resolve it the way Claude Code does, which is $CLAUDE_CONFIG_DIR, else
// ~/.claude. Injectable because a test must never depend on the developer's
// own agent history, and because the real directory here is a SYMLINK
// (~/.claude -> ~/.claudedefault) — code that assumed a literal path would
// work on this machine and nowhere else.
type Reader struct {
	Root string
}

// root resolves the Claude config directory.
func (r Reader) root() (string, error) {
	if r.Root != "" {
		return r.Root, nil
	}
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// validSessionID accepts only what a session id can be.
//
// This is a SECURITY check, not tidiness. The id arrives from Herdr's
// `agent_session.value` — not from us — and it is about to be interpolated
// into a filesystem glob. Anything containing a separator, a `..` or a glob
// metacharacter could read a file outside the transcript tree, so the rule is
// a strict allowlist of what a UUID uses: hex and dashes, bounded length.
func validSessionID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= '0' && c <= '9',
			c >= 'a' && c <= 'f',
			c >= 'A' && c <= 'F',
			c == '-':
		default:
			return false
		}
	}
	return true
}

// Find returns the transcript path for a session id.
//
// It GLOBS for the id rather than deriving the project directory from the
// agent's cwd, which is the one design decision in this file worth defending.
// The directory name is a slugified absolute path (`/a/b` -> `-a-b`), and the
// slug rule has edge cases — a leading dash on an already-slugged path
// produces a double dash, and it is not documented anywhere we control. The
// session id is a uuid and unique across the tree, so globbing finds the file
// without us having to reimplement, and keep in step with, somebody else's
// path mangling.
func (r Reader) Find(sessionID string) (string, error) {
	if !validSessionID(sessionID) {
		return "", ErrNoTranscript
	}
	root, err := r.root()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", ErrNoTranscript
	}
	// More than one match means the same session id under two project
	// directories, which should not happen. Take the most recently written:
	// it is the live one, and the alternative is to fail on an ambiguity the
	// user cannot act on.
	best, bestMod := "", time.Time{}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		if best == "" || st.ModTime().After(bestMod) {
			best, bestMod = m, st.ModTime()
		}
	}
	if best == "" {
		return "", ErrNoTranscript
	}
	return best, nil
}

// Read returns up to max events from the END of a session's transcript, oldest
// first. max <= 0 means every event in the tail that was read.
func (r Reader) Read(sessionID string, max int) ([]Event, error) {
	path, err := r.Find(sessionID)
	if err != nil {
		return nil, err
	}
	lines, err := tailLines(path)
	if err != nil {
		return nil, err
	}
	return parse(lines, max), nil
}

// tailLines reads the last tailBytes of a file and returns whole lines.
//
// The first line of the window is DISCARDED whenever the window did not start
// at byte 0, because a seek lands mid-line and half a JSON object is not a
// recoverable thing — it is a parse error per poll, forever, on exactly the
// files big enough to need the seek.
func tailLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	from := int64(0)
	if size > tailBytes {
		from = size - tailBytes
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, tailBytes+maxLineBytes))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(buf), "\n")
	if from > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// The on-disk shapes, narrowed to what is actually used. Everything else in
// those lines (and there is a great deal: cost accounting, hook traces, model
// usage, snapshots) is deliberately not decoded — a reader that only names
// what it needs does not break when the writer adds a field.
type logLine struct {
	Type        string    `json:"type"`
	IsSidechain bool      `json:"isSidechain"`
	Timestamp   time.Time `json:"timestamp"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// tool_use
	Name  string          `json:"name"`
	ID    string          `json:"id"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parse turns transcript lines into events, pairing each tool_use with its
// tool_result.
func parse(lines []string, max int) []Event {
	var out []Event
	// Where each pending tool_use landed in out, by its id, so its result can
	// be folded into the SAME event when it arrives on a later line.
	pending := map[string]int{}

	for _, raw := range lines {
		raw = strings.TrimSpace(raw)
		if raw == "" || len(raw) > maxLineBytes {
			continue
		}
		var ln logLine
		if err := json.Unmarshal([]byte(raw), &ln); err != nil {
			// A partial or unknown line is skipped, never fatal. This file is
			// being appended to by another process while we read it.
			continue
		}
		// A sidechain is a SUBAGENT's conversation. It is dropped because it is
		// not what the person is watching: the parent's `Agent` tool call
		// already stands in for the whole of it, and splicing both in produces
		// a transcript that interleaves two speakers with no way to tell them
		// apart.
		if ln.IsSidechain {
			continue
		}
		if ln.Type != "user" && ln.Type != "assistant" {
			continue
		}
		var blocks []block
		if err := json.Unmarshal(ln.Message.Content, &blocks); err != nil {
			// Content is a bare string on some user lines.
			var s string
			if json.Unmarshal(ln.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
				if txt := visibleUserText(s); txt != "" {
					out = append(out, Event{Role: RoleUser, Text: txt, At: ln.Timestamp})
				}
			}
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				role := RoleAssistant
				text := b.Text
				if ln.Type == "user" {
					role = RoleUser
					if text = visibleUserText(text); text == "" {
						continue
					}
				}
				out = append(out, Event{Role: role, Text: text, At: ln.Timestamp})
			case "tool_use":
				ev := Event{
					Role:    RoleTool,
					Tool:    b.Name,
					Summary: summarise(b.Name, b.Input),
					At:      ln.Timestamp,
				}
				out = append(out, ev)
				if b.ID != "" {
					pending[b.ID] = len(out) - 1
				}
			case "tool_result":
				i, ok := pending[b.ToolUseID]
				if !ok {
					// The call is older than our tail window. Dropping the
					// orphan is right: a result with no visible call is a
					// block of output attributed to nothing.
					continue
				}
				delete(pending, b.ToolUseID)
				res, cut := capResult(resultText(b.Content))
				out[i].Result = res
				out[i].ResultTruncated = cut
				out[i].Failed = b.IsError
			case "thinking":
				// Deliberately dropped. Measured across 464 thinking blocks on
				// this machine: every one carried a signature and NO text, so
				// there is nothing to show. A collapsed section that is always
				// empty teaches the reader to ignore collapsed sections.
				continue
			}
		}
	}

	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// visibleUserText strips the machinery that rides along on user turns.
//
// A "user" line is not only what the person typed: it also carries injected
// reminders, command wrappers and tool plumbing. Showing those verbatim makes
// the transcript look like the person said things they never said, which is
// worse than showing nothing.
func visibleUserText(s string) string {
	// <system-reminder> blocks are injected context, not speech.
	for {
		i := strings.Index(s, "<system-reminder>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</system-reminder>")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+j+len("</system-reminder>"):]
	}
	s = strings.TrimSpace(s)
	// Local command plumbing (`<command-name>`, `<local-command-stdout>`): the
	// person typed a slash command, and its expansion is not their words.
	if strings.HasPrefix(s, "<command-") || strings.HasPrefix(s, "<local-command") {
		return ""
	}
	return s
}

// resultText renders a tool_result's content, which is either a string or a
// list of content blocks.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			if x.Type == "text" && x.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(x.Text)
			}
		}
		return b.String()
	}
	return ""
}

// summaryFields names, per tool, the input field that says what the call was
// ACTUALLY about — the thing worth putting on a collapsed row.
//
// It is a table rather than a guess because the guess is wrong in the case
// that matters most: `Bash` carries both `command` and `description`, and a
// row reading "Run the tests" instead of `go test ./...` hides the only detail
// a reader is looking for.
var summaryFields = map[string][]string{
	"Bash":         {"command"},
	"Read":         {"file_path"},
	"Write":        {"file_path"},
	"Edit":         {"file_path"},
	"NotebookEdit": {"notebook_path"},
	"Glob":         {"pattern"},
	"Grep":         {"pattern"},
	"WebFetch":     {"url"},
	"WebSearch":    {"query"},
	"Task":         {"description"},
	"Agent":        {"description"},
	"Skill":        {"skill"},
	"SendMessage":  {"to"},
}

// summarise builds the one line a collapsed tool event shows.
func summarise(tool string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	try := summaryFields[tool]
	if len(try) == 0 {
		// An unknown tool (an MCP one, a new built-in) still deserves a useful
		// row, so fall back to the conventional names in rough order of how
		// specific they are.
		try = []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"}
	}
	for _, k := range try {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return oneLine(v, MaxSummaryChars)
		}
	}
	return ""
}
