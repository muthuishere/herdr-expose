package expose

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockCF is a stand-in for the Cloudflare API: enough of /zones, /cfd_tunnel
// and /dns_records to exercise the whole provisioning sequence without ever
// touching a real zone.
type mockCF struct {
	mu       sync.Mutex
	t        *testing.T
	calls    []string
	tunnels  map[string]map[string]any // name -> tunnel
	records  map[string]map[string]any // name -> record
	creates  map[string]int
	failDNS  bool
	tokenBad bool
	deleted  []string
}

func newMockCF(t *testing.T) (*mockCF, *httptest.Server) {
	m := &mockCF{t: t, tunnels: map[string]map[string]any{}, records: map[string]map[string]any{},
		creates: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	return m, srv
}

func (m *mockCF) ok(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func (m *mockCF) fail(w http.ResponseWriter, status, code int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"errors":  []map[string]any{{"code": code, "message": msg}},
	})
}

func (m *mockCF) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)

	if got := r.Header.Get("Authorization"); got != "Bearer test-token-value" {
		m.fail(w, 401, 1000, "bad token")
		return
	}
	switch {
	case r.URL.Path == "/user/tokens/verify":
		if m.tokenBad {
			m.fail(w, 401, 1000, "invalid api token")
			return
		}
		m.ok(w, map[string]any{"status": "active"})

	case r.URL.Path == "/zones":
		name := r.URL.Query().Get("name")
		if name == "example.com" {
			m.ok(w, []map[string]any{{"id": "zone123", "name": "example.com"}})
			return
		}
		m.ok(w, []map[string]any{})

	case strings.HasSuffix(r.URL.Path, "/cfd_tunnel") && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		if t, ok := m.tunnels[name]; ok {
			m.ok(w, []map[string]any{t})
			return
		}
		m.ok(w, []map[string]any{})

	case strings.HasSuffix(r.URL.Path, "/cfd_tunnel") && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["config_src"] != "local" {
			m.t.Errorf("config_src = %v, want local", body["config_src"])
		}
		if s, _ := body["tunnel_secret"].(string); len(s) < 40 {
			m.t.Errorf("tunnel_secret looks wrong: %q", s)
		}
		name, _ := body["name"].(string)
		// A REAL tunnel id is new on every creation, and that matters: a
		// deleted-and-recreated tunnel must not be mistaken for the one it
		// replaced. The first creation of a name keeps the readable id the
		// other tests assert on; later ones are distinct.
		m.creates[name]++
		id := "tun-" + name
		if n := m.creates[name]; n > 1 {
			id = fmt.Sprintf("tun-%s-%d", name, n)
		}
		tun := map[string]any{"id": id, "name": name, "account_tag": "acct1"}
		m.tunnels[name] = tun
		m.ok(w, tun)

	case strings.Contains(r.URL.Path, "/cfd_tunnel/") && r.Method == http.MethodDelete:
		id := filepath.Base(r.URL.Path)
		m.deleted = append(m.deleted, id)
		for name, t := range m.tunnels {
			if t["id"] == id {
				delete(m.tunnels, name)
			}
		}
		m.ok(w, map[string]any{"id": id})

	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		if rec, ok := m.records[name]; ok {
			m.ok(w, []map[string]any{rec})
			return
		}
		m.ok(w, []map[string]any{})

	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodPost:
		if m.failDNS {
			m.fail(w, 403, 9109, "Unauthorized to access requested resource")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		body["id"] = "rec1"
		m.records[name] = body
		m.ok(w, body)

	case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodPatch:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		body["id"] = filepath.Base(r.URL.Path)
		m.records[name] = body
		m.ok(w, body)

	case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodDelete:
		id := filepath.Base(r.URL.Path)
		for n, rec := range m.records {
			if rec["id"] == id {
				delete(m.records, n)
			}
		}
		m.deleted = append(m.deleted, id)
		m.ok(w, map[string]any{"id": id})

	default:
		m.fail(w, 404, 7003, "no route for "+r.URL.Path)
	}
}

// withMock points the provisioner at the mock API and a fake cloudflared.
func withMock(t *testing.T, srv *httptest.Server) (CloudflareOptions, *redactor) {
	t.Setenv(CloudflareTokenEnv, "test-token-value")
	t.Setenv(CloudflareAccountEnv, "acct1")
	cfAPIBaseForTest = srv.URL
	t.Cleanup(func() { cfAPIBaseForTest = "" })
	return CloudflareOptions{
		Port:       21118,
		Domain:     "herdr.example.com",
		TunnelName: "herdr-expose",
		StateDir:   t.TempDir(),
		Bin:        fakeBin(t, "cloudflared", "sleep 30"),
	}, &redactor{}
}

func TestProvisionCreatesTunnelCredentialsAndDNS(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	p, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if p.TunnelID != "tun-herdr-expose" || !p.Created {
		t.Fatalf("unexpected provision result: %+v", p)
	}

	// Credentials file: written by us, 0600, in the shape cloudflared expects.
	st, err := os.Stat(p.CredsPath)
	if err != nil {
		t.Fatalf("credentials file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials perms = %v, want 0600", st.Mode().Perm())
	}
	var creds credentialsFile
	body, _ := os.ReadFile(p.CredsPath)
	if err := json.Unmarshal(body, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.AccountTag != "acct1" || creds.TunnelID != "tun-herdr-expose" || len(creds.TunnelSecret) < 40 {
		t.Fatalf("bad credentials file: %+v", creds)
	}

	// config.yml points cloudflared at the credentials and our loopback port.
	cfg, err := os.ReadFile(p.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tunnel: tun-herdr-expose", "credentials-file: " + p.CredsPath,
		"hostname: herdr.example.com", "service: http://127.0.0.1:21118", "service: http_status:404"} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("config.yml missing %q:\n%s", want, cfg)
		}
	}

	// DNS: proxied CNAME to <tunnel>.cfargotunnel.com, tagged as ours.
	rec := mock.records["herdr.example.com"]
	if rec == nil {
		t.Fatal("no DNS record created")
	}
	if rec["type"] != "CNAME" || rec["content"] != "tun-herdr-expose.cfargotunnel.com" || rec["proxied"] != true {
		t.Fatalf("bad DNS record: %+v", rec)
	}
	if rec["comment"] != dnsComment {
		t.Fatalf("record not tagged as ours: %+v", rec)
	}

	// Preflight really did run before anything was created.
	if mock.calls[0] != "GET /user/tokens/verify" {
		t.Fatalf("first call was %q, want the token preflight", mock.calls[0])
	}

	// No secret anywhere in the generated config.yml.
	if red.contains(string(cfg)) {
		t.Fatal("a secret leaked into config.yml")
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)
	logf := func(string, ...any) {}

	first, err := provisionCloudflare(context.Background(), opts, logf, red)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provisionCloudflare(context.Background(), opts, logf, red)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if second.Created {
		t.Fatal("second run created a new tunnel instead of reusing one")
	}
	if first.TunnelID != second.TunnelID {
		t.Fatalf("tunnel id changed: %s -> %s", first.TunnelID, second.TunnelID)
	}
	if n := len(mock.tunnels); n != 1 {
		t.Fatalf("%d tunnels exist, want 1", n)
	}
	if n := countCalls(mock, "POST /zones/zone123/dns_records"); n != 1 {
		t.Fatalf("DNS record written %d times; an already-correct record must be left alone", n)
	}
}

func TestProvisionRollsBackTunnelWhenDNSFails(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)
	mock.failDNS = true

	_, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err == nil {
		t.Fatal("expected a DNS failure")
	}
	if !strings.Contains(err.Error(), "Zone:DNS:Edit") {
		t.Fatalf("error should name the missing permission, got: %v", err)
	}
	if strings.Contains(err.Error(), "test-token-value") {
		t.Fatal("the token value leaked into an error message")
	}
	if len(mock.tunnels) != 0 {
		t.Fatalf("a dangling tunnel was left behind: %+v", mock.tunnels)
	}
	if len(mock.deleted) != 1 {
		t.Fatalf("rollback did not delete the tunnel: %+v", mock.deleted)
	}
}

func TestDestroyOnlyRemovesRecordsWeCreated(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)
	logf := func(string, ...any) {}

	if _, err := provisionCloudflare(context.Background(), opts, logf, red); err != nil {
		t.Fatal(err)
	}
	// Someone else's record on the same name.
	mock.records["herdr.example.com"] = map[string]any{
		"id": "foreign", "type": "CNAME", "name": "herdr.example.com",
		"content": "somewhere.else", "proxied": true, "comment": "managed by hand",
	}
	if err := DestroyCloudflare(context.Background(), opts, logf); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, still := mock.records["herdr.example.com"]; !still {
		t.Fatal("destroy deleted a DNS record it did not create")
	}
	if len(mock.tunnels) != 0 {
		t.Fatal("destroy left the tunnel behind")
	}
}

func TestBadTokenFailsPreflightBeforeCreatingAnything(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)
	mock.tokenBad = true

	_, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err == nil {
		t.Fatal("expected preflight to fail")
	}
	if !strings.Contains(err.Error(), CloudflareTokenEnv) {
		t.Fatalf("error should name the env var, got: %v", err)
	}
	if len(mock.tunnels) != 0 || len(mock.records) != 0 {
		t.Fatal("something was provisioned despite a bad token")
	}
}

func TestMissingDomainIsRefused(t *testing.T) {
	_, err := newCloudflare(CloudflareOptions{Port: 21118}, func(string, ...any) {}, &redactor{})
	if err == nil || !strings.Contains(err.Error(), "domain is required") {
		t.Fatalf("a domainless cloudflare config must be refused, got: %v", err)
	}
}

func countCalls(m *mockCF, want string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == want {
			n++
		}
	}
	return n
}

// fakeBin writes a tiny shell script and returns its absolute path, so tests
// can exercise process supervision without a real tunnel client.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\n%s\n", body)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
