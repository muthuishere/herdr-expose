package expose

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CloudflareTokenEnv is the NAME of the environment variable holding the API
// token. The VALUE is read at the point of use and never stored in config,
// never written to a state file, never logged, never put in an error message
// and never returned from Status.
const CloudflareTokenEnv = "CLOUDFLARE_ALLPURPOSE_TOKEN"

// CloudflareAccountEnv pins the account; without it the token must see exactly
// one account.
const CloudflareAccountEnv = "CLOUDFLARE_ACCOUNT_ID"

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// cfAPIBaseForTest redirects the client at a mock server. Tests only; empty in
// every real build path.
var cfAPIBaseForTest string

// cfAPI is a small Cloudflare API client: exactly the calls needed to provision
// a named tunnel headlessly, so that `cloudflared login` — which opens a
// browser and writes an interactive cert.pem — is never run.
type cfAPI struct {
	token  string // in memory only, for the lifetime of the call chain
	base   string // overridable for tests
	client *http.Client
}

// newCFAPI reads the token by name. A missing token produces an actionable
// error that names the variable and, of course, never prints a value.
func newCFAPI(red *redactor) (*cfAPI, error) {
	tok := strings.TrimSpace(os.Getenv(CloudflareTokenEnv))
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	}
	if tok == "" {
		return nil, fmt.Errorf("no Cloudflare API token: set $%s (or $CLOUDFLARE_API_TOKEN) to a token with "+
			"Account:Cloudflare Tunnel:Edit, Zone:Zone:Read and Zone:DNS:Edit on the target zone",
			CloudflareTokenEnv)
	}
	if red != nil {
		red.add(tok)
	}
	base := cfAPIBase
	if cfAPIBaseForTest != "" {
		base = cfAPIBaseForTest
	}
	return &cfAPI{token: tok, base: base, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// apiError carries the Cloudflare error codes so callers can turn "code 9109"
// into "your token is missing Zone:DNS:Edit on example.com".
type apiError struct {
	Method, Path string
	HTTPStatus   int
	Errors       []cfError
}

func (e *apiError) Error() string {
	msgs := make([]string, 0, len(e.Errors))
	for _, c := range e.Errors {
		msgs = append(msgs, fmt.Sprintf("%d %s", c.Code, c.Message))
	}
	if len(msgs) == 0 {
		msgs = append(msgs, fmt.Sprintf("http %d", e.HTTPStatus))
	}
	return fmt.Sprintf("cloudflare api %s %s: %s", e.Method, e.Path, strings.Join(msgs, "; "))
}

// isAuth reports whether the failure is a permission problem rather than a
// bad request.
func (e *apiError) isAuth() bool {
	if e.HTTPStatus == http.StatusForbidden || e.HTTPStatus == http.StatusUnauthorized {
		return true
	}
	for _, c := range e.Errors {
		switch c.Code {
		case 9109, 10000, 1001, 7003:
			return true
		}
	}
	return false
}

func (c *cfAPI) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	// The only place the token value is ever materialised.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cloudflare api %s %s: http %d, unparseable response", method, path, resp.StatusCode)
	}
	if !env.Success {
		return &apiError{Method: method, Path: path, HTTPStatus: resp.StatusCode, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare api %s %s: decode result: %w", method, path, err)
		}
	}
	return nil
}

// verifyToken is the first preflight step: is this token valid and active?
func (c *cfAPI) verifyToken(ctx context.Context) error {
	var res struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, &res); err != nil {
		return fmt.Errorf("the Cloudflare API token in $%s is not usable: %w", CloudflareTokenEnv, err)
	}
	if res.Status != "" && res.Status != "active" {
		return fmt.Errorf("the Cloudflare API token in $%s is %q, not active", CloudflareTokenEnv, res.Status)
	}
	return nil
}

type cfAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *cfAPI) accountID(ctx context.Context) (string, error) {
	if id := strings.TrimSpace(os.Getenv(CloudflareAccountEnv)); id != "" {
		return id, nil
	}
	var accts []cfAccount
	if err := c.do(ctx, http.MethodGet, "/accounts?per_page=50", nil, &accts); err != nil {
		return "", fmt.Errorf("listing Cloudflare accounts (set $%s to skip this): %w", CloudflareAccountEnv, err)
	}
	if len(accts) == 0 {
		return "", fmt.Errorf("the Cloudflare token can see no accounts; set $%s", CloudflareAccountEnv)
	}
	if len(accts) > 1 {
		return "", fmt.Errorf("the Cloudflare token can see %d accounts; pin one with $%s", len(accts), CloudflareAccountEnv)
	}
	return accts[0].ID, nil
}

type cfTunnel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AccountTag string `json:"account_tag"`
	DeletedAt  string `json:"deleted_at"`
}

// findTunnel returns the live named tunnel with this name, if any.
func (c *cfAPI) findTunnel(ctx context.Context, account, name string) (*cfTunnel, error) {
	var list []cfTunnel
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel?name=%s&is_deleted=false", account, url.QueryEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name && list[i].DeletedAt == "" {
			return &list[i], nil
		}
	}
	return nil, nil
}

// createTunnel mints a locally-managed tunnel with a secret WE generate, so we
// can write the credentials file ourselves instead of running the interactive
// `cloudflared login`.
func (c *cfAPI) createTunnel(ctx context.Context, account, name, secretB64 string) (*cfTunnel, error) {
	body := map[string]any{
		"name":          name,
		"tunnel_secret": secretB64,
		"config_src":    "local",
	}
	var t cfTunnel
	if err := c.do(ctx, http.MethodPost, "/accounts/"+account+"/cfd_tunnel", body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *cfAPI) deleteTunnel(ctx context.Context, account, tunnelID string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/accounts/%s/cfd_tunnel/%s", account, tunnelID), nil, nil)
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// findZone walks the labels of a hostname (a.b.example.com -> b.example.com ->
// example.com) until one matches a zone the token can see.
func (c *cfAPI) findZone(ctx context.Context, hostname string) (*cfZone, error) {
	labels := strings.Split(strings.TrimSuffix(hostname, "."), ".")
	var lastErr error
	for i := 0; i+1 < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		var zones []cfZone
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(candidate), nil, &zones); err != nil {
			var ae *apiError
			if ok := asAPIError(err, &ae); ok && ae.isAuth() {
				return nil, fmt.Errorf("the Cloudflare token in $%s cannot read zones: it needs Zone:Zone:Read "+
					"on the zone containing %s (%w)", CloudflareTokenEnv, hostname, err)
			}
			lastErr = err
			continue
		}
		for j := range zones {
			if zones[j].Name == candidate {
				return &zones[j], nil
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no Cloudflare zone found for %q; check the domain and that the token in $%s has "+
		"Zone:Zone:Read on it", hostname, CloudflareTokenEnv)
}

type cfDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

func (c *cfAPI) findRecord(ctx context.Context, zoneID, name string) (*cfDNSRecord, error) {
	var recs []cfDNSRecord
	path := fmt.Sprintf("/zones/%s/dns_records?name=%s", zoneID, url.QueryEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &recs); err != nil {
		var ae *apiError
		if ok := asAPIError(err, &ae); ok && ae.isAuth() {
			return nil, fmt.Errorf("the Cloudflare token in $%s cannot read DNS records in this zone: "+
				"it needs Zone:DNS:Edit (%w)", CloudflareTokenEnv, err)
		}
		return nil, err
	}
	for i := range recs {
		if strings.EqualFold(recs[i].Name, name) {
			return &recs[i], nil
		}
	}
	return nil, nil
}

// dnsComment marks the records we created, so `expose destroy` only ever
// removes its own.
const dnsComment = "herdr-expose"

func (c *cfAPI) upsertCNAME(ctx context.Context, zoneID, name, content string, existing *cfDNSRecord) error {
	body := map[string]any{
		"type":    "CNAME",
		"name":    name,
		"content": content,
		"proxied": true,
		"ttl":     1,
		"comment": dnsComment,
	}
	var err error
	if existing == nil {
		err = c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, nil)
	} else {
		err = c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+existing.ID, body, nil)
	}
	if err != nil {
		var ae *apiError
		if ok := asAPIError(err, &ae); ok && ae.isAuth() {
			return fmt.Errorf("the Cloudflare token in $%s cannot write DNS for %s: it needs Zone:DNS:Edit (%w)",
				CloudflareTokenEnv, name, err)
		}
	}
	return err
}

func (c *cfAPI) deleteRecord(ctx context.Context, zoneID, recordID string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}

// newTunnelSecret mints the 32 random bytes that become both the API's
// tunnel_secret and the TunnelSecret in the credentials file.
func newTunnelSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate tunnel secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// asAPIError is errors.As specialised to *apiError, kept local to avoid an
// import cycle of taste.
func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
