package upstream

import (
	"encoding/json"
	"testing"
)

// Verbatim shape of `herdr session list --json` on Herdr 0.9.0 (trimmed).
const realSessionList = `{"sessions":[
 {"default":true,"name":"default","running":false,"session_dir":"/h/.config/herdr","socket_path":"/h/.config/herdr/herdr.sock"},
 {"default":false,"name":"crypto-desk","running":true,"session_dir":"/h/.config/herdr/sessions/crypto-desk","socket_path":"/h/.config/herdr/sessions/crypto-desk/herdr.sock"},
 {"default":false,"name":"herdr-plugins","running":true,"session_dir":"/h/.config/herdr/sessions/herdr-plugins","socket_path":"/h/.config/herdr/sessions/herdr-plugins/herdr.sock"},
 {"default":false,"name":"volentis","running":false,"session_dir":"/h/.config/herdr/sessions/volentis","socket_path":"/h/.config/herdr/sessions/volentis/herdr.sock"}]}`

func TestParseSessionList(t *testing.T) {
	var res sessionListResult
	if err := json.Unmarshal([]byte(realSessionList), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 4 {
		t.Fatalf("got %d sessions", len(res.Sessions))
	}
	running := RunningSessions(res.Sessions)
	if len(running) != 2 {
		t.Fatalf("got %d running, want 2", len(running))
	}
	names := map[string]bool{}
	for _, s := range running {
		names[s.Name] = true
		if s.SocketPath == "" {
			t.Fatalf("%s has no socket", s.Name)
		}
	}
	if !names["crypto-desk"] || !names["herdr-plugins"] {
		t.Fatalf("wrong running set: %v", names)
	}
}

func TestSessionNameForSocket(t *testing.T) {
	cases := map[string]string{
		"/h/.config/herdr/sessions/crypto-desk/herdr.sock": "crypto-desk",
		"/h/.config/herdr/herdr.sock":                      "default",
	}
	for sock, want := range cases {
		if got := SessionNameForSocket(sock); got != want {
			t.Fatalf("SessionNameForSocket(%q) = %q want %q", sock, got, want)
		}
	}
}

func TestSortSessionsPutsRunningFirst(t *testing.T) {
	s := []Session{{Name: "zzz", Running: false}, {Name: "b", Running: true}, {Name: "a", Running: true}}
	sortSessions(s)
	if s[0].Name != "a" || s[1].Name != "b" || s[2].Name != "zzz" {
		t.Fatalf("bad order: %v", s)
	}
}
