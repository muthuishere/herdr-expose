package core

import "testing"

// An unchanged tile is sent once, not 1.43 times a second forever. That was
// the heaviest thing this server did: a full ANSI screen per subscriber per
// tick, on an idle machine, identical to what the client already had.
func TestSummaryTileIsSentOncePerDistinctScreen(t *testing.T) {
	p := newSummaryPoller(nil, nil)
	a, b := &Session{}, &Session{}
	p.subscribe("s/w1:p1", a)

	if got := p.recipients("s/w1:p1", 111); len(got) != 1 {
		t.Fatalf("first screen went to %d subscribers, want 1", len(got))
	}
	for i := 0; i < 5; i++ {
		if got := p.recipients("s/w1:p1", 111); len(got) != 0 {
			t.Fatalf("unchanged screen resent on poll %d", i+2)
		}
	}
	if got := p.recipients("s/w1:p1", 222); len(got) != 1 {
		t.Fatalf("a changed screen was suppressed: %d recipients", len(got))
	}

	// A connection that subscribes late must get the current screen even
	// though nothing has changed since: suppression is the optimisation, the
	// first frame is the product.
	p.subscribe("s/w1:p1", b)
	got := p.recipients("s/w1:p1", 222)
	if len(got) != 1 || got[0] != b {
		t.Fatalf("late subscriber got %d frames, want its own first one", len(got))
	}
}

// Hashes are per subscriber, so one connection's state never silences another.
func TestSummaryDedupeIsPerSubscriber(t *testing.T) {
	p := newSummaryPoller(nil, nil)
	a, b := &Session{}, &Session{}
	p.subscribe("s/w1:p1", a)
	p.recipients("s/w1:p1", 7)
	p.subscribe("s/w1:p1", b)
	if got := p.recipients("s/w1:p1", 7); len(got) != 1 || got[0] != b {
		t.Fatalf("want only the new subscriber, got %d", len(got))
	}
	p.unsubscribe("s/w1:p1", a)
	if got := p.recipients("s/w1:p1", 7); len(got) != 0 {
		t.Fatalf("after unsubscribe: %d recipients", len(got))
	}
}
