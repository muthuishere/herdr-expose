package msg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MaxHops stops two agents from replying to each other forever.
const MaxHops = 8

// ErrTooManyHops is returned when a message has been forwarded MaxHops times.
var ErrTooManyHops = errors.New("hop limit reached")

// Service delivers requests to local agents and records their replies.
type Service struct {
	Store   *Store
	Herdr   Herdr
	Machine string // this machine's label, used in addresses and envelopes
	// ReplyCmd is the command the envelope tells a receiver to run. It is an
	// absolute path to this daemon's own binary, so the reply never depends on
	// a receiver's PATH (a systemd unit or ssh shell often lacks ~/.local/bin).
	ReplyCmd string
	Log      *slog.Logger
}

// SplitAddress turns "session/name" into its parts.
func SplitAddress(addr string) (session, target string, err error) {
	session, target, ok := strings.Cut(addr, "/")
	if !ok || session == "" || target == "" || strings.Contains(target, "/") {
		return "", "", fmt.Errorf("address %q: want session/agent", addr)
	}
	return session, target, nil
}

// Envelope is what the receiving agent sees. It names the request, the
// sender, and the exact command that answers it.
func Envelope(r Request, replyCmd string) string {
	if replyCmd == "" {
		replyCmd = "herdr-expose"
	}
	return fmt.Sprintf("[herdr-expose msg %s from %s, hop %d]\n"+
		"Another agent sent you this through herdr-expose messaging (see the herdr-message skill). "+
		"Do what it asks if it is within your normal work, then send your answer back by running:\n"+
		"  %s msg reply %s \"<your answer>\"\n"+
		"The sender is waiting for that reply; text you only print is never delivered.\n\n%s",
		r.ID, r.From, r.Hops, replyCmd, r.ID, r.Body)
}

// Send stores a request for a local agent and tries to deliver it at once.
func (s *Service) Send(ctx context.Context, from, to, body string, hops int) (Request, error) {
	if _, _, err := SplitAddress(to); err != nil {
		return Request{}, err
	}
	if strings.TrimSpace(body) == "" {
		return Request{}, errors.New("empty message")
	}
	if hops > MaxHops {
		return Request{}, ErrTooManyHops
	}
	r, err := s.Store.Create(from, to, body, hops)
	if err != nil {
		return Request{}, err
	}
	return s.deliver(ctx, r), nil
}

// deliver makes one attempt. Busy and blocked targets stay pending; delivery
// never answers a dialog on the target's behalf. A request already typed in is
// never typed again: its status just follows the target (blocked or not).
//
// One request is only ever in one attempt at a time. Send delivers inline and
// the retry loop ticks every couple of seconds, so both can hold the same
// request while the first is still typing — and the stored copy does not read
// "delivered" until that typing is done.
func (s *Service) deliver(ctx context.Context, r Request) Request {
	if !s.Store.Claim(r.ID) {
		return r // already being delivered; the holder records the outcome
	}
	defer s.Store.Release(r.ID)
	session, target, _ := SplitAddress(r.To)
	var status, errText string
	if r.DeliveredAt != nil {
		status, errText = s.watch(ctx, session, target)
	} else {
		status, errText = s.attempt(ctx, session, target, r)
	}
	updated, err := s.Store.Update(r.ID, func(q *Request) {
		if q.Status == StatusReplied {
			return // a reply raced the update; it wins
		}
		if q.DeliveredAt == nil {
			q.Attempts++
			if status == StatusDelivered {
				now := time.Now()
				q.DeliveredAt = &now
			}
		}
		q.Status, q.Error = status, errText
	})
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("msg: cannot record delivery", "id", r.ID, "err", err)
		}
		return r
	}
	return updated
}

// watch reports on a target that already has the message.
func (s *Service) watch(ctx context.Context, session, target string) (string, string) {
	a, err := s.find(ctx, session, target)
	switch {
	case err != nil:
		return StatusDelivered, ""
	case a == nil:
		return StatusDelivered, "target has exited; a reply may never come"
	case a.Status == "blocked":
		return StatusBlocked, "delivered, but the target is now waiting on a dialog; a human must answer it"
	}
	return StatusDelivered, ""
}

func (s *Service) find(ctx context.Context, session, target string) (*Agent, error) {
	agents, err := s.Herdr.Agents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if a := &agents[i]; a.Session == session && (a.Name == target || a.PaneID == target) {
			return a, nil
		}
	}
	return nil, nil
}

func (s *Service) attempt(ctx context.Context, session, target string, r Request) (string, string) {
	found, err := s.find(ctx, session, target)
	if err != nil {
		return StatusQueued, err.Error()
	}
	switch {
	case found == nil:
		return StatusQueued, "no agent " + r.To + " yet"
	case found.Status == "blocked":
		return StatusBlocked, "target is waiting on a dialog; a human must answer it"
	case found.Status == "working":
		return StatusQueued, "target is busy; delivers when it is idle"
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, err := s.Herdr.Prompt(pctx, session, target, Envelope(r, s.ReplyCmd))
	switch {
	case err == nil:
		return StatusDelivered, ""
	case code == "agent_blocked":
		return StatusBlocked, err.Error()
	default:
		return StatusQueued, err.Error()
	}
}

// Reply records the answer to a request.
func (s *Service) Reply(id, from, body string) (Response, error) {
	if strings.TrimSpace(body) == "" {
		return Response{}, errors.New("empty reply")
	}
	return s.Store.Reply(id, from, body)
}

// Run retries pending deliveries until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Store.Sweep()
		var reqs []Request
		for _, st := range []string{StatusQueued, StatusBlocked, StatusDelivered} {
			if l, err := s.Store.List(st); err == nil {
				reqs = append(reqs, l...)
			}
		}
		if len(reqs) == 0 {
			continue
		}
		// One agent listing per round, however many requests are waiting.
		agents, err := s.Herdr.Agents(ctx)
		round := *s
		round.Herdr = snapshot{Herdr: s.Herdr, agents: agents, err: err}
		for _, r := range reqs {
			round.deliver(ctx, r)
		}
	}
}

// snapshot answers Agents from one listing taken at the start of a round.
type snapshot struct {
	Herdr
	agents []Agent
	err    error
}

func (s snapshot) Agents(context.Context) ([]Agent, error) { return s.agents, s.err }
