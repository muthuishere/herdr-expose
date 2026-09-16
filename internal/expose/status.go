package expose

import (
	"context"
	"net/http"
	"time"
)

// Status is what `herdr-expose status`, /v1/config and the UI see. It is
// deliberately credential-free: no tokens, no env values, no command lines.
type Status struct {
	Provider string `json:"provider"` // cloudflare | ngrok | js:<id> | none
	Mode     string `json:"mode"`     // cloudflare | ngrok | js | lan | local

	// Bind is the resolved listen address: 127.0.0.1, or 0.0.0.0 in lan mode.
	Bind string `json:"bind"`
	Port int    `json:"port"`
	// LANIP is the primary non-loopback IPv4, re-resolved on SIGHUP and every
	// 30s so a DHCP lease change cannot leave a stale URL here.
	LANIP string `json:"lan_ip,omitempty"`
	// SecureContext is false for plain HTTP on a LAN IP: browsers withhold
	// service workers and PWA install there (localhost is exempt, a LAN IP is
	// not). The web app still works; it just cannot be installed.
	SecureContext bool `json:"secure_context"`
	// FellBack explains an automatic downgrade, e.g. cloudflared missing.
	FellBack string `json:"fell_back,omitempty"`
	// Tunnel is the process-backed transport, when one is running.
	Tunnel    string    `json:"tunnel,omitempty"`    // named | ngrok | js
	Running   bool      `json:"running"`             //
	Healthy   bool      `json:"healthy"`             //
	URL       string    `json:"url"`                 // public URL, empty until known
	PID       int       `json:"pid,omitempty"`       //
	Restarts  int       `json:"restarts"`            // times the process was respawned
	StartedAt time.Time `json:"started_at,omitzero"` //
	LastError string    `json:"last_error,omitempty"`
}

// tunnel is what the Manager supervises: a built-in process-backed provider or
// a JS adapter. Stop must always be idempotent.
type tunnel interface {
	Name() string
	Mode() string
	Launch(ctx context.Context) error
	WaitForURL(ctx context.Context, timeout time.Duration) (string, error)
	Snapshot() Status
	Stop() error
}

// probe does a liveness GET against <url>/healthz. Only a 200 counts: the
// Cloudflare edge answers 502/530 with a perfectly good HTTP response while the
// tunnel is not actually routing, and "the process started" is not "the tunnel
// works".
func probe(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
