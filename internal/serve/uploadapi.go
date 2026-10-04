package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PeerResolver finds a saved peer by machine name. It returns the peer's base
// URL and the token for it; ok is false when there is no such peer.
type PeerResolver func(machine string) (baseURL, token string, ok bool)

// handleUpload accepts ONE image and returns where it landed.
//
// Separate from the prompt channel on purpose. A prompt is text an agent reads;
// an image is a file an agent opens. Sending megabytes of base64 through the
// same socket that carries live screen frames would make a screenshot compete
// with the thing the person is watching, and the agent still could not open it.
// So: bytes over HTTP, path back, and the client puts the path in the prompt
// when the person sends one.
//
// The response is deliberately only {path, mime, bytes}. The client shows an
// attachment, not a path -- a 90-character temp path is noise in a text box --
// but the agent is given the path, because that is the only form it can act on.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST an image")
		return
	}
	// Paired devices only. A device token or the server token, exactly as the
	// stream requires -- an upload writes a file on the owner's machine, so it
	// is not a weaker door than reading a pane.
	if !s.uploadAuthorized(r) {
		jsonErr(w, http.StatusUnauthorized, "pair this device first")
		return
	}

	q := r.URL.Query()
	session := q.Get("session")

	// A pane on another machine needs the file on THAT machine, or the path we
	// hand the agent names nothing it can open. Forward the bytes to the peer
	// that owns it and pass its answer back unchanged.
	if m := q.Get("machine"); m != "" && s.msg != nil && m != s.msg.Machine {
		s.forwardUpload(w, r, m, session)
		return
	}

	// MaxBytesReader, not Content-Length: a length header is a claim, and the
	// limit has to hold against a body that simply keeps coming.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || len(body) > MaxUploadBytes {
			jsonErr(w, http.StatusRequestEntityTooLarge, ErrTooLarge.Error())
			return
		}
		jsonErr(w, http.StatusBadRequest, "could not read the upload")
		return
	}
	if len(body) == 0 {
		jsonErr(w, http.StatusBadRequest, "empty upload")
		return
	}

	path, mime, err := SaveUpload(UploadDir(), session, body)
	switch {
	case errors.Is(err, ErrNotAnImage):
		// Said plainly: the extension and the Content-Type were never consulted,
		// so "it is a .png" is not an argument against this.
		jsonErr(w, http.StatusUnsupportedMediaType, "only images are accepted, and the bytes are not one")
		return
	case errors.Is(err, ErrTooLarge):
		jsonErr(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		s.log.Warn("upload failed", "err", err)
		jsonErr(w, http.StatusInternalServerError, "could not store the upload")
		return
	}

	// The PATH and the SIZE, never a byte of the content.
	s.log.Info("image uploaded", "path", path, "bytes", len(body), "mime", mime,
		"ip", RemoteIP(r))
	go SweepUploads(UploadDir(), UploadTTL)

	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "mime": mime, "bytes": len(body), "machine": s.machineName(),
	})
}

func (s *Server) machineName() string {
	if s.msg != nil {
		return s.msg.Machine
	}
	return ""
}

// uploadAuthorized accepts the same identities the stream does.
func (s *Server) uploadAuthorized(r *http.Request) bool {
	if s.local.bypassAuth(r) {
		return true
	}
	tok := BearerFrom(r)
	if tok == "" {
		return false
	}
	_, err := s.auth.Authenticate(tok, RemoteIP(r), r.UserAgent())
	return err == nil
}

// forwardUpload streams the body to the peer that owns the pane.
//
// The peer writes the file on ITS disk and answers with ITS path, which is the
// only path that means anything to an agent running there. We pass that answer
// back untouched rather than rewriting it: a path invented on this machine for
// a file on another one is worse than no path at all.
func (s *Server) forwardUpload(w http.ResponseWriter, r *http.Request, machine, session string) {
	if s.Peer == nil {
		jsonErr(w, http.StatusNotImplemented, "no peers are configured on this machine")
		return
	}
	base, token, ok := s.Peer(machine)
	if !ok {
		jsonErr(w, http.StatusNotFound, "no saved peer named "+machine)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		jsonErr(w, http.StatusBadRequest, "could not read the upload")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	u := strings.TrimRight(base, "/") + "/v1/uploads?session=" + url.QueryEscape(session)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "peer "+machine+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var v map[string]any
	if json.Unmarshal(out, &v) == nil {
		if _, hasPath := v["path"]; hasPath {
			v["machine"] = machine
			s.log.Info("image uploaded to peer", "machine", machine, "path", v["path"],
				"bytes", len(body))
		}
		writeJSON(w, resp.StatusCode, v)
		return
	}
	jsonErr(w, http.StatusBadGateway, "peer "+machine+" returned an unreadable answer")
}
