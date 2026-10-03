package msg

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHerdr struct {
	agents  []Agent
	prompts []string
}

func (f *fakeHerdr) Agents(context.Context) ([]Agent, error) { return f.agents, nil }
func (f *fakeHerdr) Prompt(_ context.Context, session, target, text string) (string, error) {
	f.prompts = append(f.prompts, session+"/"+target+": "+text)
	return "", nil
}

func newTestService(t *testing.T, status string) (*Service, *fakeHerdr) {
	t.Helper()
	s, _ := newTestStore(t, 100)
	f := &fakeHerdr{agents: []Agent{{Session: "work", Name: "peer", PaneID: "w1:p1", Status: status}}}
	return &Service{Store: s, Herdr: f, Machine: "mac", Log: slog.Default()}, f
}

func TestSendDeliversToIdleAgentWithEnvelope(t *testing.T) {
	svc, f := newTestService(t, "idle")
	svc.ReplyCmd = "/opt/hx"
	r, err := svc.Send(context.Background(), "mac/work/me", "work/peer", "what dir?", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDelivered || len(f.prompts) != 1 {
		t.Fatalf("status %s, prompts %d", r.Status, len(f.prompts))
	}
	for _, want := range []string{r.ID, "mac/work/me", "what dir?", "/opt/hx msg reply " + r.ID} {
		if !strings.Contains(f.prompts[0], want) {
			t.Fatalf("envelope missing %q:\n%s", want, f.prompts[0])
		}
	}
}

func TestSendAddressesByPaneID(t *testing.T) {
	svc, f := newTestService(t, "done")
	if r, _ := svc.Send(context.Background(), "x", "work/w1:p1", "hi", 0); r.Status != StatusDelivered || len(f.prompts) != 1 {
		t.Fatalf("status %s", r.Status)
	}
}

func TestBusyTargetIsQueuedNotTyped(t *testing.T) {
	svc, f := newTestService(t, "working")
	r, _ := svc.Send(context.Background(), "x", "work/peer", "hi", 0)
	if r.Status != StatusQueued || len(f.prompts) != 0 {
		t.Fatalf("status %s, prompts %d", r.Status, len(f.prompts))
	}
	f.agents[0].Status = "idle"
	if r = svc.deliver(context.Background(), r); r.Status != StatusDelivered {
		t.Fatalf("retry: %s", r.Status)
	}
}

func TestBlockedTargetIsNeverTyped(t *testing.T) {
	svc, f := newTestService(t, "blocked")
	r, _ := svc.Send(context.Background(), "x", "work/peer", "hi", 0)
	if r.Status != StatusBlocked || len(f.prompts) != 0 {
		t.Fatalf("status %s, prompts %d", r.Status, len(f.prompts))
	}
}

func TestHopLimitAndBadInput(t *testing.T) {
	svc, _ := newTestService(t, "idle")
	if _, err := svc.Send(context.Background(), "x", "work/peer", "hi", MaxHops+1); err != ErrTooManyHops {
		t.Fatalf("hops: %v", err)
	}
	if _, err := svc.Send(context.Background(), "x", "peer", "hi", 0); err == nil {
		t.Fatal("address without session accepted")
	}
	if _, err := svc.Send(context.Background(), "x", "work/peer", "  ", 0); err == nil {
		t.Fatal("empty body accepted")
	}
}

func TestDeliveredThenBlockedIsReportedNotRetyped(t *testing.T) {
	svc, f := newTestService(t, "idle")
	r, _ := svc.Send(context.Background(), "x", "work/peer", "hi", 0)
	if r.Status != StatusDelivered || r.DeliveredAt == nil {
		t.Fatalf("status %s", r.Status)
	}
	f.agents[0].Status = "blocked"
	if r = svc.deliver(context.Background(), r); r.Status != StatusBlocked {
		t.Fatalf("after target blocked: %s", r.Status)
	}
	f.agents[0].Status = "idle"
	if r = svc.deliver(context.Background(), r); r.Status != StatusDelivered {
		t.Fatalf("after unblock: %s", r.Status)
	}
	if len(f.prompts) != 1 {
		t.Fatalf("typed %d times, want 1", len(f.prompts))
	}
}

// slowHerdr types slowly, the way the real one does: an agent listing, then a
// prompt that has to reach Herdr and be typed into a pane.
type slowHerdr struct {
	mu      sync.Mutex
	agents  []Agent
	prompts int
	typing  time.Duration
}

func (h *slowHerdr) Agents(context.Context) ([]Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agents, nil
}

func (h *slowHerdr) Prompt(_ context.Context, _, _, _ string) (string, error) {
	h.mu.Lock()
	h.prompts++
	h.mu.Unlock()
	time.Sleep(h.typing)
	return "", nil
}

func (h *slowHerdr) typed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts
}

// One send must reach the agent exactly once, even though the retry loop ticks
// several times while the first delivery is still typing. delivered_at is
// written only after the prompt returns, so for that whole window the stored
// request still reads "queued" — without a claim the loop types it again.
func TestOneSendIsTypedOnceWhileTheRetryLoopTicks(t *testing.T) {
	st, _ := newTestStore(t, 100)
	h := &slowHerdr{
		agents: []Agent{{Session: "work", Name: "peer", PaneID: "w1:p1", Status: "idle"}},
		typing: 250 * time.Millisecond,
	}
	svc := &Service{Store: st, Herdr: h, Machine: "mac", Log: slog.Default()}

	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx, 10*time.Millisecond)

	r, err := svc.Send(ctx, "mac/work/me", "work/peer", "hi", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDelivered {
		t.Fatalf("status %s, want delivered", r.Status)
	}
	time.Sleep(100 * time.Millisecond) // let any retry already past the claim land
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := h.typed(); got != 1 {
		t.Fatalf("typed %d times, want 1 — the same envelope arrived more than once", got)
	}
}
