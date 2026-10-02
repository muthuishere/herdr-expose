package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Runner is the half of a chat bridge that knows about Herdr.
//
// It is ALL of that half. The adapter it drives moves bytes and nothing else,
// so every decision lives here: which panes exist, what the menus say, which
// command a tapped button means, how an agent's screen becomes a message.
// That is the division protocol.go sets out, and the reason it is a division
// rather than a convention is that the alternative was tried -- the logic
// lived in each client, two clients grew the same bugs, and the only way to
// stop the third doing it again is to leave it nothing to get wrong.
//
// It subscribes to transcript_settled and never to anything else: the server
// already strips the terminal's furniture, folds code to markers, and holds
// the screen until the agent has stopped AND the screen has stopped moving.
// A bridge that polls raw transcript and waits on a timer is the thing this
// mode exists to replace.
type Runner struct {
	hub   *core.Hub
	store *core.Store
	id    string
	logf  Logf
	sup   *Supervisor

	mu        sync.Mutex
	sess      *core.Session
	following string // target, or "" when nothing is followed
	thread    string // the conversation that last spoke to us
	greeted   bool
}

// NewRunner wires a runner to the hub. It starts nothing.
func NewRunner(hub *core.Hub, store *core.Store, id string, logf Logf) *Runner {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Runner{hub: hub, store: store, id: id, logf: logf}
}

// Run starts the adapter and serves it until ctx ends or the supervisor gives
// up. The returned error is the supervisor's, so a caller can report the
// give-up without knowing what a chat adapter is.
func (r *Runner) Run(ctx context.Context, spec Spec) error {
	r.sup = NewSupervisor(spec, r.logf, r.onAdapterLine)

	// One hub session for the whole bridge. It is the thing that receives
	// transcript frames, and it is closed when the bridge stops so a crashed
	// adapter cannot leave a subscription polling a pane forever.
	sess := r.hub.NewSession(ctx, r)
	r.mu.Lock()
	r.sess = sess
	r.mu.Unlock()
	defer sess.Close()

	return r.sup.Run(ctx)
}

/* ---------------------------------------------------------------- the sink */

// SendBinary satisfies core.ClientSink and is deliberately a no-op.
//
// Binary frames are a live terminal stream for an emulator. A chat app has no
// emulator, which is exactly why this bridge never declares `live` -- and a
// sink that silently discarded a stream it HAD asked for would be a bug, so
// the guarantee is upstream: nothing here ever subscribes to one.
func (r *Runner) SendBinary(*upstream.Buf) {}

// SendJSON receives the control plane. Only transcript frames matter.
func (r *Runner) SendJSON(typ string, data any) {
	if typ != "transcript" {
		return
	}
	f, ok := data.(core.TranscriptFrame)
	if !ok {
		return
	}
	r.mu.Lock()
	following, thread := r.following, r.thread
	r.mu.Unlock()

	if f.Target != following {
		return
	}
	text := strings.TrimRight(f.Text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	// The frame is already settled, cleaned and folded. Adding anything to it
	// here would be this half formatting on the adapter's behalf, which is the
	// thing the adapter is forbidden from doing to the host.
	r.send(Outbound{
		Type:    "send",
		Text:    fmt.Sprintf("%s\n\n%s", short(f.Target), text),
		Thread:  thread,
		Choices: paneChoices(),
	})
}

/* ------------------------------------------------------------- the adapter */

func (r *Runner) send(o Outbound) {
	if r.sup == nil {
		return
	}
	if err := r.sup.Send(o); err != nil {
		r.logf("chat %s: send failed: %v", r.id, err)
	}
}

// onAdapterLine handles one protocol line from the adapter's stdout.
func (r *Runner) onAdapterLine(line []byte) {
	var in Inbound
	if err := decode(line, &in); err != nil {
		// A line that is not protocol is the adapter printing to the wrong
		// stream. Say so once, with the line, because the author's next
		// question is "why did nothing happen".
		r.logf("chat %s: ignoring non-protocol line on stdout: %s", r.id, trim(string(line), 120))
		return
	}
	switch in.Type {
	case "ready":
		r.greet(in.Thread)
	case "message":
		r.handle(in)
	}
}

func (r *Runner) greet(thread string) {
	r.mu.Lock()
	if thread != "" {
		r.thread = thread
	}
	already := r.greeted
	r.greeted = true
	t := r.thread
	r.mu.Unlock()
	if already {
		return
	}
	r.send(Outbound{
		Type:   "send",
		Thread: t,
		Text:   "herdr bridge up\n/ls to list panes · tap one to follow it · then just type to send a prompt",
	})
}

// handle is the whole command surface. A tapped button arrives here as that
// choice's Data, so there is exactly one implementation of every command.
func (r *Runner) handle(in Inbound) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return
	}
	r.mu.Lock()
	if in.Thread != "" {
		r.thread = in.Thread
	}
	thread := r.thread
	following := r.following
	r.mu.Unlock()

	switch {
	case text == "/ls" || text == "/help":
		r.sendPanes(thread)

	case strings.HasPrefix(text, "p:"):
		r.follow(strings.TrimPrefix(text, "p:"), thread)

	case strings.HasPrefix(text, "k:"):
		r.key(following, strings.TrimPrefix(text, "k:"), thread)

	default:
		r.prompt(following, text, thread)
	}
}

func (r *Runner) sendPanes(thread string) {
	panes := r.agentPanes()
	if len(panes) == 0 {
		r.send(Outbound{Type: "send", Thread: thread, Text: "no panes"})
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d pane(s)\n", len(panes))
	choices := make([]Choice, 0, len(panes))
	for _, p := range panes {
		state := p.state
		if state == "" {
			state = "shell"
		}
		fmt.Fprintf(&b, "  [p:%s] %s — %s\n", p.target, short(p.target), state)
		choices = append(choices, Choice{
			Label: fmt.Sprintf("%s · %s", short(p.target), state),
			Data:  "p:" + p.target,
		})
	}
	// The text ALREADY lists every option with its payload, so an adapter
	// without buttons is not degraded: it shows the same list and the same
	// strings work when typed.
	r.send(Outbound{Type: "send", Thread: thread, Text: strings.TrimRight(b.String(), "\n"), Choices: choices})
}

func (r *Runner) follow(target, thread string) {
	r.mu.Lock()
	sess := r.sess
	r.following = target
	r.mu.Unlock()
	if sess == nil {
		return
	}
	// transcript_settled, and only this target. Declaring one target at a time
	// is what keeps a chat window from becoming the mirror this bridge exists
	// not to be.
	sess.SetViewport(map[string]string{target: string(core.ModeTranscriptSettled)})
	r.send(Outbound{
		Type:   "send",
		Thread: thread,
		Text:   fmt.Sprintf("following %s — its screen arrives when it settles", short(target)),
	})
}

func (r *Runner) prompt(target, text, thread string) {
	if target == "" {
		r.send(Outbound{Type: "send", Thread: thread, Text: "nothing followed yet — /ls, then pick a pane"})
		return
	}
	session, pane := splitTarget(target)
	// The parameter is `target`, NOT `pane_id`: api.md uses pane_id for other
	// methods, so the wrong one is accepted, delivers nothing, and the only
	// symptom is an answer that never comes.
	if err := r.call(session, "agent.prompt", map[string]any{"target": pane, "text": text}); err != nil {
		r.send(Outbound{Type: "send", Thread: thread, Text: "could not send: " + err.Error()})
		return
	}
	r.send(Outbound{Type: "send", Thread: thread, Text: "sent to " + short(target)})
}

func (r *Runner) key(target, key, thread string) {
	if target == "" {
		return
	}
	session, pane := splitTarget(target)
	if err := r.call(session, "agent.send_keys", map[string]any{"target": pane, "keys": key}); err != nil {
		r.send(Outbound{Type: "send", Thread: thread, Text: "could not send key: " + err.Error()})
	}
}

func (r *Runner) call(session, method string, params map[string]any) error {
	c := r.store.Client(session)
	if c == nil {
		return fmt.Errorf("no session %q", session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err := c.Call(ctx, method, params)
	return err
}

/* ----------------------------------------------------------------- helpers */

type paneInfo struct {
	target string
	state  string
}

func (r *Runner) agentPanes() []paneInfo {
	tree := r.store.Tree()
	if tree == nil {
		return nil
	}
	var out []paneInfo
	for _, s := range tree.Sessions {
		if s.Snapshot == nil {
			continue
		}
		// An agent is bound to a TERMINAL, not to a pane id, so the state has
		// to be looked up that way round -- matching on pane id alone quietly
		// loses every agent in a split.
		state := map[string]string{}
		for _, a := range s.Snapshot.Agents {
			if a.PaneID != "" {
				state[a.PaneID] = a.AgentStatus
			}
		}
		for _, p := range s.Snapshot.Panes {
			out = append(out, paneInfo{
				target: s.Name + core.TargetSep + p.PaneID,
				state:  state[p.PaneID],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].target < out[j].target })
	return out
}

// paneChoices are the buttons offered alongside a settled screen: the answers
// an agent's question usually needs, plus the way back to the list.
func paneChoices() []Choice {
	return []Choice{
		{Label: "y", Data: "k:y"},
		{Label: "n", Data: "k:n"},
		{Label: "enter", Data: "k:enter"},
		{Label: "panes", Data: "/ls"},
	}
}

func splitTarget(t string) (session, pane string) {
	if i := strings.Index(t, core.TargetSep); i >= 0 {
		return t[:i], t[i+len(core.TargetSep):]
	}
	return "", t
}

// short drops the session prefix: a chat window is narrow and the session is
// the same for every line in it.
func short(t string) string {
	_, p := splitTarget(t)
	if p == "" {
		return t
	}
	return p
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
