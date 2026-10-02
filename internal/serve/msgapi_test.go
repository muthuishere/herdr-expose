package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/msg"
)

type stubHerdr struct{ prompts int }

func (h *stubHerdr) Agents(context.Context) ([]msg.Agent, error) {
	return []msg.Agent{{Address: "work/peer", Session: "work", Name: "peer", PaneID: "w1:p1", Status: "idle"}}, nil
}
func (h *stubHerdr) Prompt(context.Context, string, string, string) (string, error) {
	h.prompts++
	return "", nil
}

const testMsgToken = "hxm_test"

func newMsgTestServer(t *testing.T) (*Server, *stubHerdr) {
	t.Helper()
	s := newTestServer(t)
	st, err := msg.NewStore(t.TempDir(), 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &stubHerdr{}
	s.msg = &msg.Service{Store: st, Herdr: h, Machine: "mac"}
	s.msgToken = testMsgToken
	return s, h
}

// do sends a request the way a peer daemon does: through a hostname that is
// NOT configured here, with no Origin, carrying only a bearer token.
func do(s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://unconfigured.peer.example"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestMsgAPIRequiresTheMessagingToken(t *testing.T) {
	s, h := newMsgTestServer(t)
	device := pairDevice(t, s.auth, "phone")
	for _, tc := range []struct {
		name, token string
	}{
		{"no token", ""},
		{"wrong token", "hxm_nope"},
		{"a paired device token (view links must not send)", device},
	} {
		for _, r := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/agents", ""},
			{http.MethodPost, "/v1/messages", `{"to":"work/peer","body":"hi"}`},
		} {
			if rec := do(s, r.method, r.path, tc.token, r.body); rec.Code < 400 {
				t.Errorf("%s: %s %s = %d, want refused", tc.name, r.method, r.path, rec.Code)
			}
		}
	}
	if h.prompts != 0 {
		t.Fatalf("an unauthorized request typed into an agent %d times", h.prompts)
	}
}

func TestMsgAPIRoundTrip(t *testing.T) {
	s, h := newMsgTestServer(t)

	rec := do(s, http.MethodGet, "/v1/agents", testMsgToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"work/peer"`) {
		t.Fatalf("agents: %d %s", rec.Code, rec.Body)
	}

	rec = do(s, http.MethodPost, "/v1/messages", testMsgToken, `{"from":"x","to":"work/peer","body":"hi"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var req msg.Request
	_ = json.Unmarshal(rec.Body.Bytes(), &req)
	if req.Status != msg.StatusDelivered || h.prompts != 1 {
		t.Fatalf("send: status %s, prompts %d", req.Status, h.prompts)
	}

	rec = do(s, http.MethodPost, "/v1/messages/"+req.ID+"/reply", testMsgToken, `{"from":"peer","body":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	if rec = do(s, http.MethodPost, "/v1/messages/"+req.ID+"/reply", testMsgToken, `{"from":"peer","body":"again"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second reply: %d, want 409", rec.Code)
	}

	rec = do(s, http.MethodGet, "/v1/messages/"+req.ID, testMsgToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec = do(s, http.MethodGet, "/v1/messages/r-0000000000001-abcd", testMsgToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
}

func TestOtherRoutesKeepHostPinning(t *testing.T) {
	s, _ := newMsgTestServer(t)
	// The messaging exemption must not leak to the rest of the API.
	if rec := do(s, http.MethodGet, "/v1/config", testMsgToken, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("/v1/config from an unconfigured host with a bearer: %d, want 403", rec.Code)
	}
}

// The handshake must announce every mode the parser accepts.
//
// This pins the bug it was written after: welcome.viewport.modes listed four
// modes while core.ParseMode accepted seven, so transcript_clean,
// transcript_prose and transcript_settled worked perfectly for anyone told
// about them and were invisible to everyone else. docs/api.md calls this list
// "the authoritative list", which is exactly what makes an omission a lie
// rather than an oversight.
//
// It is written as a round-trip rather than against a hardcoded list, so
// adding a mode to core cannot leave this file behind: a new Mode constant
// that ParseMode accepts and the handshake withholds fails here.
func TestAdvertisedModesCoverEveryParsedMode(t *testing.T) {
	adv := advertisedModes()

	// Every advertised string must parse back to itself. A typo would
	// otherwise advertise a mode that silently resolves to `none` -- the
	// blank-pane failure the docs warn clients about.
	for _, m := range adv {
		if got := core.ParseMode(m); string(got) != m {
			t.Errorf("advertised %q parses to %q, not itself", m, got)
		}
	}

	// And every mode core knows must be advertised.
	for _, m := range []core.Mode{
		core.ModeLive, core.ModeTranscript, core.ModeTranscriptClean,
		core.ModeTranscriptProse, core.ModeTranscriptSettled,
		core.ModeSummary, core.ModeNone,
	} {
		if !slices.Contains(adv, string(m)) {
			t.Errorf("core accepts %q but the handshake does not advertise it", m)
		}
	}

	// Every transcript flavour must agree with core about being one, so a
	// flavour cannot be advertised while the no-geometry contract skips it.
	for _, m := range adv {
		mode := core.ParseMode(m)
		if strings.HasPrefix(m, "transcript") != mode.IsTranscript() {
			t.Errorf("%q: name says transcript=%v, IsTranscript()=%v",
				m, strings.HasPrefix(m, "transcript"), mode.IsTranscript())
		}
	}
}
