package serve

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/muthuishere/herdr-expose/internal/agentlog"
	"github.com/muthuishere/herdr-expose/internal/core"
)

// defaultAgentLogEvents is how many events a client gets when it does not ask.
//
// 60, which at the measured median (190 characters of prose, a 268-byte tool
// result) is a few tens of KB — a readable scrollback on a phone without
// sending a conversation nobody is going to scroll to.
const defaultAgentLogEvents = 60

// maxAgentLogEvents caps what a client may ask for. The cap exists because the
// per-event cost is bounded but not small: 400 events each carrying a 4KB
// result is 1.6MB, and this endpoint is reachable from a phone on a LAN.
const maxAgentLogEvents = 400

// handleAgentLog serves an agent's own structured transcript (ADR 0040).
//
// It is a SEPARATE endpoint rather than another field on the `transcript`
// stream frame, deliberately. The stream is the fanout path, and ADR 0014's
// budget is about exactly that path: a shared poller pushing tens of KB of
// markdown and tool output to every subscriber on every tick would spend the
// streaming budget on history nobody is reading. This is fetched when somebody
// opens a pane, which is when they want it.
//
// The `session` parameter is the agent's own session id — the same value the
// UI already has as `agent.id` on a pane, which is where Herdr's
// `agent_session.value` lands (see view.go). So the client does not have to
// learn a second addressing scheme.
//
// AUTHORISATION. The id is NOT sufficient authority on its own, and treating
// it as such would be a scope bypass: on a share pinned to one session, a
// client that learned another session's uuid could otherwise read that
// agent's whole conversation, which is strictly more than the share was ever
// allowed to show. So the id is resolved back to the pane that owns it and the
// SAME scope gate the rest of the API uses is applied to that pane. An id that
// matches no visible pane is answered exactly as an unknown one is.
func (s *Server) handleAgentLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session is required"})
		return
	}

	// Resolve the id to a pane we are allowed to show, BEFORE touching the
	// filesystem. A refusal must not depend on whether the file exists, or the
	// 404/403 split becomes an oracle for which sessions are running.
	target, ok := targetForAgentSession(s.hub.Store().Tree(), s.hub.Store().Scope(), sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no agent with that session id is visible from here",
		})
		return
	}

	limit := defaultAgentLogEvents
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be a positive number"})
			return
		}
		limit = min(n, maxAgentLogEvents)
	}

	events, err := agentlog.Reader{}.Read(sessionID, limit)
	if err != nil {
		if errors.Is(err, agentlog.ErrNoTranscript) {
			// NOT an error for the client. An agent that has not spoken yet,
			// a non-Claude agent, and a cleared history all land here, and in
			// every case the right behaviour is to fall back to the pane. So
			// this is a 200 that says "nothing here", which a client can act
			// on without special-casing a status code.
			writeJSON(w, http.StatusOK, map[string]any{
				"target":    target,
				"available": false,
				"events":    []agentlog.Event{},
			})
			return
		}
		s.log.Warn("agent log read failed", "target", target, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the transcript"})
		return
	}
	if events == nil {
		events = []agentlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":    target,
		"available": true,
		"events":    events,
	})
}

// targetForAgentSession finds the pane whose agent carries this session id and
// reports whether this instance's scope allows showing it.
//
// It is a PURE FUNCTION of the tree, the scope and the id — not a method — so
// that the authorisation rule can be tested directly against a constructed
// tree. An authorisation check reachable only through an HTTP handler and a
// live Herdr connection is an authorisation check nobody tests.
//
// It walks the live tree rather than consulting the filesystem, which is what
// makes it an authorisation check at all: the question is not "does this
// transcript exist" but "is its pane one this client may see".
func targetForAgentSession(tree *core.Tree, scope *core.Scope, sessionID string) (string, bool) {
	if tree == nil || sessionID == "" {
		return "", false
	}
	for _, sess := range tree.Sessions {
		if sess == nil || sess.Snapshot == nil {
			continue
		}
		if scope.Active() && !scope.AllowsSession(sess.Name) {
			continue
		}
		for _, p := range sess.Snapshot.Panes {
			if p.AgentSession == nil || p.AgentSession.Value != sessionID {
				continue
			}
			target := sess.Name + "/" + p.PaneID
			if scope.Active() && !scope.AllowsTarget(target) {
				// Found, and refused. Keep looking in case the same agent is
				// visible under a target the scope does allow; a bare `return`
				// here would hide a pane this client is entitled to see.
				continue
			}
			return target, true
		}
	}
	return "", false
}
