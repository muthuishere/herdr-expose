package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tapped button and a typed command must be the SAME event.
//
// The adapter sends a choice's Data back as Text, so there is exactly one
// implementation of every command. The alternative -- a callback path beside
// the text path -- is two code paths that must agree forever, and they stop
// agreeing the first time somebody adds a command to one of them.
func TestChoiceDataIsALiteralCommand(t *testing.T) {
	for _, c := range paneChoices() {
		if c.Data == "" {
			t.Fatalf("choice %q has no data; a button that sends nothing does nothing", c.Label)
		}
		// Every payload must be something handle() can route: a command, or a
		// prefixed selector. A label that happens to look like prose would be
		// typed into an agent as a prompt.
		ok := strings.HasPrefix(c.Data, "k:") || strings.HasPrefix(c.Data, "p:") ||
			strings.HasPrefix(c.Data, "/")
		if !ok {
			t.Errorf("choice %q data %q is not a routable command", c.Label, c.Data)
		}
	}
}

// Text must carry the options too. An adapter with no buttons then shows the
// same list, and the same strings work when typed -- which is why a plain
// adapter is not a degraded one and its author never writes a text fallback.
func TestPaneListTextContainsEveryChoicePayload(t *testing.T) {
	// Build the same way sendPanes does, without a hub.
	panes := []paneInfo{
		{target: "work/w1:p1", state: "idle"},
		{target: "work/w2:p1", state: "blocked"},
	}
	var b strings.Builder
	var choices []Choice
	for _, p := range panes {
		b.WriteString("[p:" + p.target + "] ")
		choices = append(choices, Choice{Label: p.target, Data: "p:" + p.target})
	}
	for _, c := range choices {
		if !strings.Contains(b.String(), c.Data) {
			t.Errorf("text does not contain %q, so a button-less adapter cannot offer it", c.Data)
		}
	}
}

// short() drops the session prefix, because a chat window is narrow and every
// line in it has the same session.
func TestShortAndSplitTarget(t *testing.T) {
	s, p := splitTarget("openjevx/w1:p1")
	if s != "openjevx" || p != "w1:p1" {
		t.Fatalf("splitTarget = %q,%q", s, p)
	}
	if got := short("openjevx/w1:p1"); got != "w1:p1" {
		t.Errorf("short = %q, want w1:p1", got)
	}
	// A target with no session must survive rather than become empty: an
	// empty pane id would be sent to Herdr as a prompt against nothing.
	if got := short("w1:p1"); got != "w1:p1" {
		t.Errorf("short of a bare pane = %q, want it unchanged", got)
	}
}

// A frame is encoded as ONE line. A multi-line frame would be read by the
// adapter as several, each one invalid JSON.
func TestEncodeIsExactlyOneLine(t *testing.T) {
	line, err := Encode(Outbound{
		Type:    "send",
		Text:    "first\nsecond\nthird",
		Choices: []Choice{{Label: "y", Data: "k:y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(line), "\n"); n != 1 {
		t.Fatalf("frame has %d newlines, want exactly 1 (the terminator)", n)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Error("frame does not end in a newline")
	}
	// And it round-trips with the embedded newlines intact.
	var back Outbound
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.Text != "first\nsecond\nthird" {
		t.Errorf("text came back as %q", back.Text)
	}
}
