package expose

import (
	"strings"
	"sync"
)

// redactor holds the set of secret VALUES this process has touched (a
// Cloudflare API token, a tunnel token, an ngrok authtoken, anything an adapter
// pulled out of the environment with ctx.env()) and scrubs them out of every
// string on its way to a log line, a status payload or a state file.
//
// Secrets are used, never shown: the value lives in memory and in a child
// process's environment, and nowhere else.
type redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// add registers a secret value. Short values are ignored: redacting a 3-char
// string would mangle unrelated output for no security benefit.
func (r *redactor) add(secret string) {
	s := strings.TrimSpace(secret)
	if len(s) < 8 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if existing == s {
			return
		}
	}
	r.secrets = append(r.secrets, s)
}

// scrub replaces every known secret in s with a marker.
func (r *redactor) scrub(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.secrets) == 0 {
		return s
	}
	out := s
	for _, sec := range r.secrets {
		if strings.Contains(out, sec) {
			out = strings.ReplaceAll(out, sec, "[redacted]")
		}
	}
	return out
}

// contains reports whether s carries any known secret. Used by tests and by the
// status path as a last line of defence.
func (r *redactor) contains(s string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, sec := range r.secrets {
		if strings.Contains(s, sec) {
			return true
		}
	}
	return false
}
