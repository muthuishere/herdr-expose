package msg

import (
	"context"
	"log/slog"
	"strings"
	"testing"
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
