package serve

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/muthuishere/herdr-expose/internal/msg"
)

// Messaging API. It has its own token, separate from view-only share and
// device tokens: a link for WATCHING a pane can never inject messages.
//
//	GET  /v1/agents                  every agent in every running session here
//	POST /v1/messages                {from, to, body, hops} → request (with id)
//	GET  /v1/messages[?status=]      requests, newest first
//	GET  /v1/messages/{id}           {request, response?}
//	POST /v1/messages/{id}/reply     {from, body} → response

func isMsgPath(p string) bool {
	return p == "/v1/agents" || p == "/v1/inventory" ||
		p == "/v1/messages" || strings.HasPrefix(p, "/v1/messages/")
}

func (s *Server) msgAuthorized(r *http.Request) bool {
	tok := BearerFrom(r)
	return s.msgToken != "" && tok != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(s.msgToken)) == 1
}

func jsonErr(w http.ResponseWriter, code int, err string) {
	writeJSON(w, code, map[string]string{"error": err})
}

func (s *Server) handleMsg(w http.ResponseWriter, r *http.Request) {
	if s.msg == nil {
		jsonErr(w, http.StatusNotFound, "messaging is not enabled")
		return
	}
	if !s.msgAuthorized(r) {
		jsonErr(w, http.StatusUnauthorized, "messaging token required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	path := r.URL.Path
	switch {
	case path == "/v1/agents" && r.Method == http.MethodGet:
		agents, err := s.msg.Herdr.Agents(r.Context())
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if agents == nil {
			agents = []msg.Agent{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"machine": s.msg.Machine, "agents": agents})

	case path == "/v1/inventory" && r.Method == http.MethodGet:
		// Who is working, who is free, who stopped on an error, and who has
		// done nothing long enough to be worth closing. `agents` says what
		// EXISTS; this says what it MEANS, which is what a caller acts on.
		if s.Inventory == nil {
			jsonErr(w, http.StatusNotFound, "inventory is not enabled")
			return
		}
		sum, err := s.Inventory(r.Context(), r.URL.Query().Get("self"))
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sum)

	case path == "/v1/messages" && r.Method == http.MethodPost:
		var in struct {
			From string `json:"from"`
			To   string `json:"to"`
			Body string `json:"body"`
			Hops int    `json:"hops"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			jsonErr(w, http.StatusBadRequest, "bad json")
			return
		}
		req, err := s.msg.Send(r.Context(), in.From, in.To, in.Body, in.Hops)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, req)

	case path == "/v1/messages" && r.Method == http.MethodGet:
		reqs, err := s.msg.Store.List(r.URL.Query().Get("status"))
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": reqs})

	case strings.HasPrefix(path, "/v1/messages/"):
		rest := strings.TrimPrefix(path, "/v1/messages/")
		id, action, _ := strings.Cut(rest, "/")
		switch {
		case action == "" && r.Method == http.MethodGet:
			req, resp, err := s.msg.Store.Get(id)
			if errors.Is(err, msg.ErrNotFound) {
				jsonErr(w, http.StatusNotFound, err.Error())
				return
			}
			if err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"request": req, "response": resp})
		case action == "reply" && r.Method == http.MethodPost:
			var in struct {
				From string `json:"from"`
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				jsonErr(w, http.StatusBadRequest, "bad json")
				return
			}
			resp, err := s.msg.Reply(id, in.From, in.Body)
			if errors.Is(err, msg.ErrNotFound) {
				jsonErr(w, http.StatusNotFound, err.Error())
				return
			}
			if err != nil {
				jsonErr(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
		default:
			jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
