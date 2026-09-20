package core

import (
	"context"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Transcript mode (SPEC AMENDMENTS 13).
//
// What this is: a ~1Hz poll of a pane's VISIBLE BUFFER, ANSI stripped, pushed
// to the client as a control-plane JSON frame.
//
// What it is NOT, and the client must never imply otherwise: a conversation
// log. Herdr exposes the SCREEN. There is no message history to read, so this
// carries no message boundaries, no roles and no turns — only the text the
// agent is currently showing, plus a truthful statement of which buffer it came
// from. Inventing structure here would be worse than the terminal mirror it
// replaces, because it would look authoritative while being fabricated.
const (
	// TranscriptInterval is the poll period. 1Hz is fast enough to feel live
	// for reading and slow enough that it is not the summary firehose again.
	TranscriptInterval = 1 * time.Second
	// TranscriptLines is how much recent buffer to ask for.
	//
	// It was 400, and 400 was chosen on the theory that a bigger read is free
	// because it is deduplicated across subscribers. MEASURED on an isolated
	// 12-session bed (herdr 0.9.0, over the socket, which is the only way this
	// process ever reads): `pane.read source=recent_unwrapped` costs 0.1ms at
	// 0 lines, 0.1ms at 100, 0.2ms at 400 and 0.3ms at 1600 — flat, and the
	// same flat with 60k lines of scrollback behind the pane. So the line
	// budget is NOT a latency lever upstream; it is a PAYLOAD lever. 400 lines
	// was ~8.5KB of JSON per subscriber per second per pane, for text no phone
	// renders. 200 halves that and is still more history than the view shows.
	//
	// (The ~780ms/call the stress run attributed to this read could not be
	// reproduced on the bed at any line count, by socket or by CLI. What IS
	// reproducible, and what actually made the view go stale, is the serial
	// sweep below: whatever one read costs, the period was N times it.)
	TranscriptLines = 200

	// TranscriptWorkers bounds how many targets are read AT ONCE in one sweep.
	//
	// The sweep used to be a plain `for _, target := range targets { poll() }`
	// inside a 1s ticker, so the real period was N x (cost of one read) and the
	// product's DEFAULT view went stale in direct proportion to how many panes
	// you were watching — the one thing a user cannot do anything about.
	// A bounded pool makes the period max(TranscriptInterval, cost x N/W)
	// instead, and bounded is the point: unbounded fanout across 44 panes is a
	// thundering herd on somebody's real Herdr server, which K5 spends a whole
	// amendment keeping calm. Each read dials its own short-lived socket
	// connection (upstream.Client.Call), so concurrency here is safe.
	TranscriptWorkers = 8

	// SourceRecent is the default: recent output as LOGICAL lines, not as the
	// PTY's post-layout grid rows. Herdr spells it with an underscore on the
	// socket even though the CLI flag is hyphenated; sending the CLI spelling
	// is an `unknown variant` error, not a fallback.
	SourceRecent = "recent_unwrapped"
	// SourceDetection is the bottom-buffer region Herdr itself classifies on.
	// A blocked agent's question lives there, and it is the same text the
	// existing Q&A panel renders, so the two never disagree.
	SourceDetection = "detection"
)

// TranscriptFrame is the `transcript` control-plane frame's data.
//
// Source and Agent are on the wire deliberately: the UI has to be able to say
// WHAT it is showing, and it cannot do that from the text alone.
type TranscriptFrame struct {
	Target string `json:"target"`
	// Source is the Herdr buffer this text came from, verbatim.
	Source string `json:"source"`
	// Text is plain UTF-8 with every escape sequence removed. It is the
	// agent's own output and our chrome is never mixed into it.
	Text string `json:"text"`
	// Lines is the line budget the read asked for.
	Lines int `json:"lines,omitempty"`
	// Truncated is Herdr's own report that the buffer had more than we asked
	// for. Surfaced rather than swallowed: "this is the tail" is a fact the
	// reader needs.
	Truncated bool `json:"truncated,omitempty"`
	// Agent is true when the target has an agent bound to it.
	Agent bool `json:"agent,omitempty"`
	// State is the agent's state at read time, or "" for a plain pane.
	State string `json:"state,omitempty"`
	// At is when the read happened.
	At time.Time `json:"at"`
}

// transcriptSub is one connection's subscription to one target.
type transcriptSub struct {
	// hash of the last text actually delivered, so an unchanged screen costs
	// nothing on the wire. A 1Hz unconditional push is the mistake that made
	// 26 panes × a full screen every 0.65s into 206KB/s.
	hash uint64
	sent bool
}

// transcriptPoller deduplicates transcript reads across connections, exactly as
// summaryPoller does for tiles — the read is geometry-free, so one poll serves
// every subscriber. Change detection is PER SUBSCRIBER, because a connection
// that has just subscribed needs the current text even though nothing changed.
type transcriptPoller struct {
	store *Store
	log   *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*Session]*transcriptSub
	wake chan struct{}
}

func newTranscriptPoller(store *Store, log *slog.Logger) *transcriptPoller {
	return &transcriptPoller{store: store, log: log,
		subs: map[string]map[*Session]*transcriptSub{}, wake: make(chan struct{}, 1)}
}

func (p *transcriptPoller) subscribe(target string, s *Session) {
	p.mu.Lock()
	m := p.subs[target]
	if m == nil {
		m = map[*Session]*transcriptSub{}
		p.subs[target] = m
	}
	if _, ok := m[s]; !ok {
		m[s] = &transcriptSub{}
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *transcriptPoller) unsubscribe(target string, s *Session) {
	p.mu.Lock()
	if m := p.subs[target]; m != nil {
		delete(m, s)
		if len(m) == 0 {
			delete(p.subs, target)
		}
	}
	p.mu.Unlock()
}

func (p *transcriptPoller) unsubscribeAll(s *Session) {
	p.mu.Lock()
	for target, m := range p.subs {
		delete(m, s)
		if len(m) == 0 {
			delete(p.subs, target)
		}
	}
	p.mu.Unlock()
}

func (p *transcriptPoller) run(ctx context.Context) {
	t := time.NewTicker(TranscriptInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-t.C:
		}
		p.mu.Lock()
		targets := make([]string, 0, len(p.subs))
		for target := range p.subs {
			targets = append(targets, target)
		}
		p.mu.Unlock()
		p.sweep(ctx, targets)
	}
}

// sweep reads every subscribed target through a bounded worker pool and returns
// only when the whole sweep is done, so sweeps never overlap themselves.
//
// This is the fix for the staleness: the period of the DEFAULT view is now
// max(TranscriptInterval, readCost x ceil(N/TranscriptWorkers)) instead of
// N x readCost.
func (p *transcriptPoller) sweep(ctx context.Context, targets []string) {
	forEachBounded(ctx, targets, TranscriptWorkers, p.poll)
}

// forEachBounded runs fn over items with at most `workers` in flight, and
// returns only when every item has been handled (or ctx is done). Bounded, not
// unbounded: the items here are upstream reads against somebody's real Herdr
// server, and 44 of them at once is a thundering herd.
func forEachBounded(ctx context.Context, items []string, workers int,
	fn func(context.Context, string)) {
	if len(items) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if len(items) < workers {
		workers = len(items)
	}
	work := make(chan string)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for item := range work {
				fn(ctx, item)
			}
		}()
	}
	for _, item := range items {
		select {
		case work <- item:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
}

func (p *transcriptPoller) poll(ctx context.Context, target string) {
	// Resolve FIRST: it is the one place scope is enforced, so an out-of-scope
	// target is unreachable here exactly as it is on every other live path.
	_, c, id, err := p.store.Resolve(target)
	if err != nil {
		return
	}

	pane := p.store.Tree().Pane(target)
	state := ""
	if pane != nil {
		state = pane.AgentStatus
	}
	// Herdr reports `agent_status: "unknown"` for every plain shell pane
	// (verified on 0.9.0), so the presence of the FIELD says nothing at all.
	// Only a status that means something is worth addressing via agent.read.
	hasAgent := pane != nil && (pane.Agent != "" || (state != "" && state != "unknown"))

	// A blocked agent is READ FROM THE DETECTION REGION. `recent_unwrapped`
	// would show the question too, but detection is the region Herdr itself
	// classified as the prompt, so the transcript and the answer keys are
	// always looking at the same thing.
	source := SourceRecent
	lines := TranscriptLines
	if isBlockedStatus(state) {
		source = SourceDetection
		lines = 0
	}

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	var res upstream.PaneReadResult
	if hasAgent {
		// agent.read, addressed by pane id — the agent-facing read, which is
		// what SPEC J3 names. It returns the same pane_read payload.
		//
		// It can still refuse: a pane can carry agent STATE that came from
		// `herdr pane report-agent` (any process may declare its own
		// lifecycle) without Herdr having an agent object to read from. That
		// pane is exactly the one a human most needs to see, so a refusal
		// falls through to pane.read rather than leaving the view empty. The
		// buffer is the same buffer either way.
		if err = c.AgentRead(rctx, id, source, lines, &res); err != nil {
			err = c.PaneReadFull(rctx, id, source, lines, &res)
		}
	} else {
		err = c.PaneReadFull(rctx, id, source, lines, &res)
	}
	cancel()
	if err != nil {
		p.log.Debug("transcript read failed", "target", target, "err", err)
		return
	}

	// ANSI is stripped SERVER-side. The client renders text, never an emulator.
	text := strings.TrimRight(StripANSI(res.Read.Text), "\n \t")

	frame := TranscriptFrame{
		Target: target, Source: source, Text: text, Lines: lines,
		Truncated: res.Read.Truncated, Agent: hasAgent, State: state,
		At: time.Now(),
	}
	// The identity is the text PLUS what it is: the same characters read from
	// `detection` instead of `recent_unwrapped`, or read while the agent's
	// state changed, are a different thing to show and must not be suppressed
	// as "unchanged".
	p.deliver(target, frame, source+"\x00"+state+"\x00"+text)
}

// deliver applies per-subscriber change detection and fans the frame out.
//
// Change detection is PER SUBSCRIBER, not per target. A shared "last sent"
// would mean a connection that subscribed a moment ago sees nothing until the
// agent happens to move — an empty pane on a phone, indistinguishable from a
// broken one. Suppression is the optimisation; the first frame is the product.
func (p *transcriptPoller) deliver(target string, frame TranscriptFrame, identity string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(identity))
	sum := h.Sum64()

	p.mu.Lock()
	out := make([]*Session, 0, len(p.subs[target]))
	for s, sub := range p.subs[target] {
		if sub.sent && sub.hash == sum {
			continue
		}
		sub.hash = sum
		sub.sent = true
		out = append(out, s)
	}
	p.mu.Unlock()

	for _, s := range out {
		s.sink.SendJSON("transcript", frame)
	}
}

// isBlockedStatus mirrors serve.agentState's blocked arm. It is duplicated
// rather than imported because core must not depend on serve, and it is two
// literals.
func isBlockedStatus(status string) bool {
	return status == "blocked" || status == "waiting"
}
