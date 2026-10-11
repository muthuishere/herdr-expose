package agentlog

import (
	"os"
	"strings"
	"testing"
)

// A live read against this machine's REAL transcripts.
//
// Skipped by default, because it depends on the developer's own agent history
// and would fail on any other machine and in CI — which is exactly why it is
// also worth having. Every fixture in claude_test.go is a statement about a
// format somebody else owns and can change without telling us; this is the
// only test here that can notice when that happens.
//
//	HEX_AGENTLOG_SESSION=<uuid> go test ./internal/agentlog/ -run Live -v
//
// Get the uuid the way the product will:
//
//	printf '{"jsonrpc":"2.0","id":"1","method":"agent.list","params":{}}\n' \
//	  | nc -U "$HERDR_SOCKET_PATH" | jq -r '.result.agents[].agent_session.value'
func TestLiveTranscriptReadsAndLooksLikeAConversation(t *testing.T) {
	sid := strings.TrimSpace(os.Getenv("HEX_AGENTLOG_SESSION"))
	if sid == "" {
		t.Skip("set HEX_AGENTLOG_SESSION=<claude session uuid> to run this")
	}

	path, err := Reader{}.Find(sid)
	if err != nil {
		t.Fatalf("Find(%s): %v", sid, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript: %s (%d KB)", path, st.Size()/1024)

	evs, err := Reader{}.Read(sid, 40)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("a live session produced no events")
	}

	var roles = map[Role]int{}
	markdown, summarised, capped := 0, 0, 0
	for _, e := range evs {
		roles[e.Role]++
		if strings.Contains(e.Text, "**") || strings.Contains(e.Text, "\n## ") ||
			strings.Contains(e.Text, "```") || strings.Contains(e.Text, "- ") {
			markdown++
		}
		if e.Role == RoleTool {
			if e.Summary != "" {
				summarised++
			}
			if e.ResultTruncated {
				capped++
			}
			if e.Tool == "" {
				t.Errorf("a tool event with no tool name: %+v", e)
			}
			if len(e.Result) > MaxToolResultBytes {
				t.Errorf("a result got past the cap: %d bytes", len(e.Result))
			}
		}
		// Whatever else is true, nothing may be BOTH prose and a tool call:
		// the client renders those two completely differently.
		if e.Role == RoleTool && e.Text != "" {
			t.Errorf("event is both a tool call and prose: %+v", e)
		}
	}
	t.Logf("events=%d roles=%v with-markdown=%d tools-with-a-summary=%d capped-results=%d",
		len(evs), roles, markdown, summarised, capped)

	// The whole justification for this package is that the terminal had no
	// markdown and the transcript does. If a live read finds none, either this
	// session genuinely has none or the premise has stopped being true, and
	// both are worth being told about.
	if markdown == 0 {
		t.Log("WARNING: no markdown in the last 40 events of a live session — " +
			"check the premise in the package comment still holds")
	}
	if roles[RoleTool] > 0 && summarised == 0 {
		t.Error("every tool event came back with an empty summary; a collapsed " +
			"list of 800 rows reading \"Bash\" is useless")
	}
}
