package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/msg"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Messaging. The daemon on each machine owns the requests for ITS agents; this
// command is a thin client of a daemon: the local one, or a peer's.
//
// Addresses:  session/agent            an agent on this machine
//             peer:session/agent       an agent on a saved peer
// Request ids from a peer come back as peer:r-…, and wait/get follow them there.

const msgTokenFile = "msg-token" // in the state dir, 0600
const peersFile = "peers.json"   // in the state dir, 0600

// msgToken returns this machine's messaging token, minting it once.
func msgToken(state string) (string, error) {
	p := filepath.Join(state, msgTokenFile)
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := "hxm_" + hex.EncodeToString(b[:])
	return tok, os.WriteFile(p, []byte(tok+"\n"), 0o600)
}

// machineName labels this machine in addresses and envelopes.
func machineName() string {
	if m := os.Getenv("HERDR_EXPOSE_MACHINE"); m != "" {
		return m
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	if h == "" {
		h = "local"
	}
	return strings.ToLower(h)
}

// newMsgService builds the daemon side.
//
// client is how delivery reaches a session, and passing one is what keeps this
// off the herdr CLI. A CLI invocation ATTACHES to the terminal session at its
// own size, which SIGWINCHes the pane and makes the agent's TUI redraw -- the
// "screen keeps scrolling" bug. nil falls back to the CLI, which is what a
// caller without a store (the one-shot `msg` subcommand) still needs.
func newMsgService(state, herdrBin string, client func(string) msg.Caller) (*msg.Service, string, error) {
	tok, err := msgToken(state)
	if err != nil {
		return nil, "", err
	}
	st, err := msg.NewStore(filepath.Join(state, "messages"), msg.DefaultKeep, msg.DefaultExpiry)
	if err != nil {
		return nil, "", err
	}
	reply := os.Getenv("HERDR_EXPOSE_REPLY_CMD")
	if reply == "" {
		if exe, err := os.Executable(); err == nil {
			reply = exe
		}
	}
	var backend msg.Herdr = msg.CLI{Bin: herdrBin}
	if client != nil {
		backend = msg.Socket{Client: client}
	}
	return &msg.Service{Store: st, Herdr: backend, Machine: machineName(), ReplyCmd: reply}, tok, nil
}

type peer struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func loadPeers(state string) (map[string]peer, error) {
	m := map[string]peer{}
	b, err := os.ReadFile(filepath.Join(state, peersFile))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func savePeers(state string, m map[string]peer) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(state, peersFile), append(b, '\n'), 0o600)
}

// endpoint is one daemon we can talk to.
type endpoint struct {
	name, url, token string
}

func (e endpoint) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.url, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w (is the daemon running there?)", e.name, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e2 struct{ Error string }
		if json.Unmarshal(b, &e2) == nil && e2.Error != "" {
			return fmt.Errorf("%s: %s", e.name, e2.Error)
		}
		return fmt.Errorf("%s: HTTP %d", e.name, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type msgCtx struct {
	state string
	local endpoint
	peers map[string]peer
}

func newMsgCtx() (*msgCtx, error) {
	state, err := StateDir()
	if err != nil {
		return nil, err
	}
	store, err := config.Open()
	if err != nil {
		return nil, err
	}
	tok, err := msgToken(state)
	if err != nil {
		return nil, err
	}
	peers, err := loadPeers(state)
	if err != nil {
		return nil, err
	}
	return &msgCtx{state: state, peers: peers,
		local: endpoint{name: "local daemon", url: fmt.Sprintf("http://127.0.0.1:%d", store.Port()), token: tok}}, nil
}

// route splits "peer:rest" into the endpoint and rest.
func (c *msgCtx) route(s string) (endpoint, string, string, error) {
	if name, rest, ok := strings.Cut(s, ":"); ok && !strings.Contains(name, "/") {
		if p, ok := c.peers[name]; ok {
			return endpoint{name: name, url: p.URL, token: p.Token}, rest, name + ":", nil
		}
		// a bare pane id like w1:p2 is not a peer; let it fall through
		if !strings.HasPrefix(rest, "p") {
			return endpoint{}, "", "", fmt.Errorf("unknown peer %q (herdr-expose msg peer list)", name)
		}
	}
	return c.local, s, "", nil
}

// self is this agent's own address, for the from field: the agent's Herdr
// name when it has one (what a human and the skill both address it by),
// otherwise its pane id.
func self() string {
	s, p := os.Getenv("HERDR_SESSION"), os.Getenv("HERDR_PANE_ID")
	if s == "" {
		s = "default"
	}
	if p == "" {
		return machineName() + "/" + s + "/(outside herdr)"
	}
	return machineName() + "/" + s + "/" + agentName(s, p)
}

// agentName asks Herdr for the name of the agent in pane, falling back to the
// pane id. Best effort and quick: a slow or absent Herdr must not stop a send.
func agentName(session, pane string) string {
	bin, err := upstream.ResolveHerdrBin()
	if err != nil {
		return pane
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--session", session, "agent", "get", pane).Output()
	if err != nil {
		return pane
	}
	var v struct {
		Result struct {
			Agent struct {
				Name string `json:"name"`
			} `json:"agent"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &v) != nil || v.Result.Agent.Name == "" {
		return pane
	}
	return v.Result.Agent.Name
}

func runMsg(args []string) error {
	if len(args) == 0 {
		return errors.New(msgUsage)
	}
	c, err := newMsgCtx()
	if err != nil {
		return err
	}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	switch args[0] {
	case "token":
		fmt.Println(c.local.token)
		return nil

	case "peer":
		return c.peerCmd(args[1:])

	case "agents", "list":
		return c.agents(len(args) > 1 && args[1] == "--all")

	case "send":
		if len(args) < 3 {
			return errors.New("usage: herdr-expose msg send <[peer:]session/agent> <text> [--wait SECONDS]")
		}
		ep, to, prefix, err := c.route(args[1])
		if err != nil {
			return err
		}
		var r msg.Request
		if err := ep.do("POST", "/v1/messages", map[string]any{"from": self(), "to": to, "body": args[2]}, &r); err != nil {
			return err
		}
		fmt.Printf("%s%s  %s", prefix, r.ID, r.Status)
		if r.Error != "" {
			fmt.Printf("  (%s)", r.Error)
		}
		fmt.Println()
		if len(args) >= 5 && args[3] == "--wait" {
			var secs int
			fmt.Sscan(args[4], &secs)
			return c.wait(prefix+r.ID, time.Duration(secs)*time.Second)
		}
		return nil

	case "wait":
		if len(args) < 2 {
			return errors.New("usage: herdr-expose msg wait <[peer:]id> [SECONDS]")
		}
		secs := 300
		if len(args) > 2 {
			fmt.Sscan(args[2], &secs)
		}
		return c.wait(args[1], time.Duration(secs)*time.Second)

	case "get":
		if len(args) < 2 {
			return errors.New("usage: herdr-expose msg get <[peer:]id>")
		}
		ep, id, _, err := c.route(args[1])
		if err != nil {
			return err
		}
		var v any
		if err := ep.do("GET", "/v1/messages/"+id, nil, &v); err != nil {
			return err
		}
		return out.Encode(v)

	case "reply":
		if len(args) < 3 {
			return errors.New("usage: herdr-expose msg reply <id> <text>")
		}
		var r msg.Response
		if err := c.local.do("POST", "/v1/messages/"+args[1]+"/reply", map[string]any{"from": self(), "body": args[2]}, &r); err != nil {
			return err
		}
		fmt.Println("replied", r.ID)
		return nil

	case "inbox":
		var v struct{ Requests []msg.Request }
		if err := c.local.do("GET", "/v1/messages", nil, &v); err != nil {
			return err
		}
		for _, r := range v.Requests {
			fmt.Printf("%s  %-9s  %s → %s  %q\n", r.ID, r.Status, r.From, r.To, trunc(r.Body, 60))
		}
		return nil
	}
	return errors.New(msgUsage)
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *msgCtx) wait(ref string, max time.Duration) error {
	ep, id, _, err := c.route(ref)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(max)
	for {
		var v struct {
			Request  msg.Request   `json:"request"`
			Response *msg.Response `json:"response"`
		}
		if err := ep.do("GET", "/v1/messages/"+id, nil, &v); err != nil {
			return err
		}
		if v.Response != nil {
			fmt.Printf("reply from %s:\n%s\n", v.Response.From, v.Response.Body)
			return nil
		}
		switch v.Request.Status {
		case msg.StatusFailed, msg.StatusExpired:
			return fmt.Errorf("%s: %s %s", ref, v.Request.Status, v.Request.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: no reply yet (status %s %s)", ref, v.Request.Status, v.Request.Error)
		}
		time.Sleep(time.Second)
	}
}

func (c *msgCtx) agents(all bool) error {
	type src struct {
		label string
		ep    endpoint
	}
	srcs := []src{{"", c.local}}
	if all {
		names := make([]string, 0, len(c.peers))
		for n := range c.peers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			srcs = append(srcs, src{n + ":", endpoint{name: n, url: c.peers[n].URL, token: c.peers[n].Token}})
		}
	}
	for _, s := range srcs {
		var v struct {
			Machine string
			Agents  []msg.Agent
		}
		if err := s.ep.do("GET", "/v1/agents", nil, &v); err != nil {
			fmt.Printf("# %s unreachable: %v\n", s.ep.name, err)
			continue
		}
		fmt.Printf("# %s (%s)\n", v.Machine, strings.TrimSuffix(s.label, ":")+map[bool]string{true: "this machine", false: ""}[s.label == ""])
		for _, a := range v.Agents {
			fmt.Printf("%-40s %-9s %-8s %s\n", s.label+a.Address, a.Kind, a.Status, a.Cwd)
		}
	}
	return nil
}

func (c *msgCtx) peerCmd(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		for n, p := range c.peers {
			fmt.Printf("%-16s %s\n", n, p.URL)
		}
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) != 4 {
			return errors.New("usage: herdr-expose msg peer add <name> <url> <token>")
		}
		c.peers[args[1]] = peer{URL: args[2], Token: args[3]}
		if err := savePeers(c.state, c.peers); err != nil {
			return err
		}
		ep := endpoint{name: args[1], url: args[2], token: args[3]}
		var v struct{ Machine string }
		if err := ep.do("GET", "/v1/agents", nil, &v); err != nil {
			fmt.Printf("saved %s, but it is not reachable yet: %v\n", args[1], err)
			return nil
		}
		fmt.Printf("saved %s → %s (machine %s)\n", args[1], args[2], v.Machine)
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: herdr-expose msg peer remove <name>")
		}
		delete(c.peers, args[1])
		return savePeers(c.state, c.peers)
	}
	return errors.New("usage: herdr-expose msg peer [list|add|remove]")
}

const msgUsage = `usage: herdr-expose msg <command>
  agents [--all]                 agents on this machine (--all: and every peer)
  send <[peer:]session/agent> <text> [--wait SECONDS]
                                 send; prints the request id
  wait <[peer:]id> [SECONDS]     block until the reply arrives (default 300)
  get <[peer:]id>                request and response
  reply <id> <text>              answer a message you received
  inbox                          requests held by this machine
  token                          this machine's messaging token (give it to peers)
  peer list | add <name> <url> <token> | remove <name>`
