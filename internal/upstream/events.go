package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"time"
)

// EventSink receives upstream events and connection-state transitions.
//
// Resync is called immediately AFTER the subscription is confirmed and BEFORE
// any event is delivered. 0.9.0 no longer replays retained history on
// events.subscribe, so the only correct order is subscribe -> snapshot. The
// caller takes the snapshot inside Resync; anything that happened before it is
// already in the snapshot, anything after arrives as an event.
type EventSink interface {
	Resync()
	OnEvent(Event)
	OnUpstreamState(connected bool, err error)
}

// EventStream keeps one long-lived socket connection subscribed to Herdr's
// session-wide events, reconnecting with exponential backoff.
type EventStream struct {
	client *Client
	sink   EventSink
	subs   []string
	log    *slog.Logger
}

// NewEventStream builds a stream. subs nil => GlobalSubscriptions.
func NewEventStream(c *Client, sink EventSink, log *slog.Logger, subs []string) *EventStream {
	if subs == nil {
		subs = GlobalSubscriptions
	}
	if log == nil {
		log = slog.Default()
	}
	return &EventStream{client: c, sink: sink, subs: subs, log: log}
}

// Run blocks until ctx is cancelled, holding the subscription open.
func (s *EventStream) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	const maxBackoff = 15 * time.Second
	for ctx.Err() == nil {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.sink.OnUpstreamState(false, err)
		if err != nil {
			s.log.Warn("upstream event stream dropped", "err", err, "retry_in", backoff)
		}
		jitter := time.Duration(rand.Int63n(int64(backoff/4) + 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// session holds one connection for as long as it lives.
func (s *EventStream) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := s.client.dial(dialCtx)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	subs := make([]map[string]string, 0, len(s.subs))
	for _, t := range s.subs {
		subs = append(subs, map[string]string{"type": t})
	}
	req, _ := json.Marshal(Request{
		ID:     "events",
		Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs},
	})
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{})

	br := bufio.NewReaderSize(conn, 1<<16)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := readLine(br)
	if err != nil {
		return err
	}
	var ack Response
	if err := json.Unmarshal(first, &ack); err != nil {
		return err
	}
	if ack.Error != nil {
		return ack.Error
	}
	_ = conn.SetReadDeadline(time.Time{})

	// CRITICAL ORDER: subscription is live, now take the tree snapshot.
	s.sink.OnUpstreamState(true, nil)
	s.sink.Resync()

	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			s.log.Debug("upstream: unparseable event line", "err", err)
			continue
		}
		if ev.Event == "" {
			continue // late response envelope, not an event
		}
		s.sink.OnEvent(ev)
	}
}
