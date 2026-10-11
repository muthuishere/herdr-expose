package agentlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript lays out a fake Claude config tree and returns its root.
func writeTranscript(t *testing.T, sessionID string, lines ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "-some-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// A tool call and its result are ONE event, because that is how they are read.
func TestAToolCallAndItsResultAreOneEvent(t *testing.T) {
	root := writeTranscript(t, sid,
		`{"type":"assistant","timestamp":"2026-10-11T10:00:00Z","message":{"content":[`+
			`{"type":"text","text":"I'll run the tests."},`+
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"Run the tests"}}]}}`,
		`{"type":"user","timestamp":"2026-10-11T10:00:05Z","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"t1","content":"ok   all tests passed"}]}}`,
	)
	evs, err := Reader{Root: root}.Read(sid, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events (prose + one tool), got %d: %+v", len(evs), evs)
	}
	if evs[0].Role != RoleAssistant || evs[0].Text != "I'll run the tests." {
		t.Fatalf("prose event wrong: %+v", evs[0])
	}
	tool := evs[1]
	if tool.Role != RoleTool || tool.Tool != "Bash" {
		t.Fatalf("tool event wrong: %+v", tool)
	}
	// The COMMAND, not the description: a row reading "Run the tests" hides
	// the one detail a reader is looking for.
	if tool.Summary != "go test ./..." {
		t.Fatalf("summary = %q, want the command", tool.Summary)
	}
	if !strings.Contains(tool.Result, "all tests passed") {
		t.Fatalf("result not folded into the call: %+v", tool)
	}
}

// A 431KB result was measured on this machine. It must not reach a phone.
func TestAHugeToolResultIsCappedAndSaysSo(t *testing.T) {
	huge := strings.Repeat("x234567890\n", 10000) // ~110KB: over the 4KB cap, inside the 512KB tail window
	root := writeTranscript(t, sid,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat big"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":`+jsonString(huge)+`}]}}`,
	)
	evs, err := Reader{Root: root}.Read(sid, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if len(evs[0].Result) > MaxToolResultBytes {
		t.Fatalf("result is %d bytes, cap is %d", len(evs[0].Result), MaxToolResultBytes)
	}
	if !evs[0].ResultTruncated {
		t.Fatal("the cap bit but ResultTruncated is false; a reader cannot tell a short result from a cut one")
	}
	// Cut on a line boundary, so it does not read as corruption.
	if strings.HasSuffix(evs[0].Result, "x2345") {
		t.Fatal("result was cut mid-line")
	}
}

// Subagent conversations are not what the person is watching.
func TestSidechainsAreDropped(t *testing.T) {
	root := writeTranscript(t, sid,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"parent speaking"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"SUBAGENT CHATTER"}]}}`,
	)
	evs, _ := Reader{Root: root}.Read(sid, 0)
	for _, e := range evs {
		if strings.Contains(e.Text, "SUBAGENT") {
			t.Fatalf("a sidechain reached the transcript: %+v", e)
		}
	}
	if len(evs) != 1 {
		t.Fatalf("want only the parent event, got %d", len(evs))
	}
}

// Injected context must never be shown as something the person said.
func TestInjectedContextIsNotAttributedToTheUser(t *testing.T) {
	root := writeTranscript(t, sid,
		`{"type":"user","message":{"content":[{"type":"text","text":"do the thing<system-reminder>SECRET POLICY TEXT</system-reminder>"}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"<command-name>/compact</command-name>"}]}}`,
	)
	evs, _ := Reader{Root: root}.Read(sid, 0)
	if len(evs) != 1 {
		t.Fatalf("want 1 visible user event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Text != "do the thing" {
		t.Fatalf("user text = %q; the reminder should be gone and the words kept", evs[0].Text)
	}
}

// Markdown must arrive as SOURCE. The whole point of this package is that the
// client gets to render it, because only the client knows the screen width.
func TestMarkdownArrivesAsSourceNotRendered(t *testing.T) {
	md := "## Heading\n\n**bold** and `code`\n\n```go\nfmt.Println(1)\n```"
	root := writeTranscript(t, sid,
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+jsonString(md)+`}]}}`,
	)
	evs, _ := Reader{Root: root}.Read(sid, 0)
	if len(evs) != 1 || evs[0].Text != md {
		t.Fatalf("markdown did not survive verbatim: %q", evs[0].Text)
	}
}

// A result whose call is older than the tail window is an orphan: output
// attributed to nothing. Dropped rather than shown.
func TestAnOrphanToolResultIsDropped(t *testing.T) {
	root := writeTranscript(t, sid,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"gone","content":"output with no call"}]}}`,
	)
	evs, _ := Reader{Root: root}.Read(sid, 0)
	if len(evs) != 0 {
		t.Fatalf("an orphan result was shown: %+v", evs)
	}
}

// The session id goes into a GLOB, so it is validated as a security boundary:
// it arrives from Herdr, not from us.
func TestASessionIDCannotEscapeTheTranscriptTree(t *testing.T) {
	root := writeTranscript(t, sid, `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)
	r := Reader{Root: root}
	for _, bad := range []string{
		"../../../etc/passwd",
		"*",
		"../*/*",
		"aaaa/bbbb",
		"aaaa*bbbb",
		`aaaa\bbbb`,
		"",
		"zzz", // too short
	} {
		if _, err := r.Find(bad); err == nil {
			t.Fatalf("session id %q was accepted; it must not reach a glob", bad)
		}
	}
}

// A missing transcript is NORMAL and must be distinguishable, so callers fall
// back to the pane instead of showing an error.
func TestAMissingTranscriptIsANamedConditionNotAnError(t *testing.T) {
	root := t.TempDir()
	_, err := Reader{Root: root}.Read("bbbbbbbb-cccc-dddd-eeee-ffffffffffff", 0)
	if err == nil {
		t.Fatal("want ErrNoTranscript, got nil")
	}
	if !strings.Contains(err.Error(), "no transcript") {
		t.Fatalf("want ErrNoTranscript, got %v", err)
	}
}

// A seek lands mid-line, and half a JSON object is a parse error forever on
// exactly the files big enough to need the seek.
func TestATailSeekDoesNotChokeOnThePartialFirstLine(t *testing.T) {
	var lines []string
	// Comfortably over tailBytes so the seek path is the one under test.
	for i := 0; i < 4000; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"assistant","message":{"content":[{"type":"text","text":%s}]}}`,
			jsonString(strings.Repeat("padding ", 20)+fmt.Sprintf("line-%d", i))))
	}
	root := writeTranscript(t, sid, lines...)
	evs, err := Reader{Root: root}.Read(sid, 5)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) != 5 {
		t.Fatalf("want the last 5 events, got %d", len(evs))
	}
	// The LAST line of the file must be present: the tail is the point.
	if !strings.Contains(evs[len(evs)-1].Text, "line-3999") {
		t.Fatalf("tail did not reach the end of the file: %q", evs[len(evs)-1].Text)
	}
	for _, e := range evs {
		if strings.TrimSpace(e.Text) == "" {
			t.Fatal("an empty event came out of the seek window")
		}
	}
}

// max keeps the NEWEST events, not the oldest: a transcript view opens at the
// bottom.
func TestMaxKeepsTheNewestEvents(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"assistant","message":{"content":[{"type":"text","text":"e%d"}]}}`, i))
	}
	root := writeTranscript(t, sid, lines...)
	evs, _ := Reader{Root: root}.Read(sid, 3)
	if len(evs) != 3 || evs[0].Text != "e7" || evs[2].Text != "e9" {
		t.Fatalf("want the last three events e7..e9, got %+v", evs)
	}
}

// Empty thinking blocks are dropped: 464 measured, every one signature-only.
// A collapsed section that is always empty teaches people to ignore them.
func TestEmptyThinkingIsNotAnEvent(t *testing.T) {
	root := writeTranscript(t, sid,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"abc"},{"type":"text","text":"said"}]}}`,
	)
	evs, _ := Reader{Root: root}.Read(sid, 0)
	if len(evs) != 1 || evs[0].Text != "said" {
		t.Fatalf("want only the spoken text, got %+v", evs)
	}
}

// jsonString quotes a Go string as a JSON string literal, for building fixtures.
func jsonString(s string) string {
	b, err := jsonMarshal(s)
	if err != nil {
		panic(err)
	}
	return b
}

func jsonMarshal(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}
