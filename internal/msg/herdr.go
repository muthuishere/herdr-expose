package msg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Agent is one agent in one Herdr session on this machine.
type Agent struct {
	Address string `json:"address"` // session/name, or session/pane when unnamed
	Session string `json:"session"`
	Name    string `json:"name,omitempty"`
	PaneID  string `json:"pane_id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title,omitempty"`
}

// Herdr is what delivery needs from Herdr. The real one shells out to the
// herdr CLI, which is Herdr's documented, versioned automation surface.
type Herdr interface {
	Agents(ctx context.Context) ([]Agent, error)
	// Prompt submits text to session/target. code is Herdr's error code
	// (agent_blocked, agent_not_found, ...) when it refuses.
	Prompt(ctx context.Context, session, target, text string) (code string, err error)
}

// CLI is the Herdr implementation backed by the herdr binary.
type CLI struct{ Bin string }

func (c CLI) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

type herdrAgent struct {
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	Title       string `json:"terminal_title_stripped"`
}

// Agents lists agents across every running Herdr session.
func (c CLI) Agents(ctx context.Context) ([]Agent, error) {
	all, err := upstream.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Agent
	for _, s := range upstream.RunningSessions(all) {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		stdout, _, err := c.run(ctx2, "--session", s.Name, "agent", "list")
		cancel()
		if err != nil {
			continue // one dead session must not hide the others
		}
		var res struct {
			Result struct {
				Agents []herdrAgent `json:"agents"`
			} `json:"result"`
		}
		if json.Unmarshal(stdout, &res) != nil {
			continue
		}
		for _, a := range res.Result.Agents {
			id := a.Name
			if id == "" {
				id = a.PaneID
			}
			out = append(out, Agent{Address: s.Name + "/" + id, Session: s.Name,
				Name: a.Name, PaneID: a.PaneID, Kind: a.Agent, Status: a.AgentStatus,
				Cwd: a.Cwd, Title: a.Title})
		}
	}
	return out, nil
}

// Prompt submits text plus Enter through `herdr agent prompt`.
func (c CLI) Prompt(ctx context.Context, session, target, text string) (string, error) {
	_, stderr, err := c.run(ctx, "--session", session, "agent", "prompt", target, text)
	if err == nil {
		return "", nil
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(stderr), &e) == nil && e.Error.Code != "" {
		return e.Error.Code, fmt.Errorf("%s: %s", e.Error.Code, e.Error.Message)
	}
	return "", fmt.Errorf("herdr agent prompt: %v: %s", err, strings.TrimSpace(string(stderr)))
}
