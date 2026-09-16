package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// fakeLister lets the store be driven without live Herdr servers. The sockets
// do not exist, so each worker's event stream simply fails and backs off — which
// is exactly the "session is unreachable" case we want the tree to survive.
type fakeLister struct {
	mu   sync.Mutex
	list []upstream.Session
}

func (f *fakeLister) set(l []upstream.Session) {
	f.mu.Lock()
	f.list = l
	f.mu.Unlock()
}

func (f *fakeLister) List(context.Context) ([]upstream.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstream.Session(nil), f.list...), nil
}

func quietStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitFor(t *testing.T, s *Store, cond func(*Tree) bool) *Tree {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tr := s.Tree(); cond(tr) {
			return tr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met; tree = %+v", s.Tree())
	return nil
}

// A session appearing must be picked up with no restart, and a session going
// away must NOT disturb the others: its entry stays, marked not running.
func TestSessionsAppearAndDisappear(t *testing.T) {
	s := quietStore(t)
	f := &fakeLister{list: []upstream.Session{
		{Name: "alpha", SocketPath: "/nonexistent/alpha.sock", Running: true},
	}}
	s.Registry().List = f.List
	s.SetRegistryInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, s, func(tr *Tree) bool { return tr.Session("alpha") != nil })
	if s.Client("alpha") == nil {
		t.Fatal("no client for alpha")
	}

	// beta appears while the server is running.
	f.set([]upstream.Session{
		{Name: "alpha", SocketPath: "/nonexistent/alpha.sock", Running: true},
		{Name: "beta", SocketPath: "/nonexistent/beta.sock", Running: true},
	})
	waitFor(t, s, func(tr *Tree) bool { return tr.Session("beta") != nil })
	if s.Client("beta") == nil {
		t.Fatal("no client for beta")
	}

	// alpha dies. beta must be untouched and alpha must survive as an entry.
	f.set([]upstream.Session{
		{Name: "alpha", SocketPath: "/nonexistent/alpha.sock", Running: false},
		{Name: "beta", SocketPath: "/nonexistent/beta.sock", Running: true},
	})
	tr := waitFor(t, s, func(tr *Tree) bool {
		a := tr.Session("alpha")
		return a != nil && !a.Running
	})
	if b := tr.Session("beta"); b == nil || !b.Running {
		t.Fatalf("beta was disturbed by alpha dying: %+v", b)
	}
	if s.Client("beta") == nil {
		t.Fatal("beta's client was torn down when alpha died")
	}
}

// Targets must route to their own session's client, and an unknown session must
// be a clean error rather than a silent fallback to somebody else's socket.
func TestResolveRoutesBySession(t *testing.T) {
	s := quietStore(t)
	f := &fakeLister{list: []upstream.Session{
		{Name: "alpha", SocketPath: "/nonexistent/alpha.sock", Running: true},
		{Name: "beta", SocketPath: "/nonexistent/beta.sock", Running: true},
	}}
	s.Registry().List = f.List
	s.SetRegistryInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, s, func(tr *Tree) bool { return tr.Session("beta") != nil })

	name, c, id, err := s.Resolve("beta/w1:p1")
	if err != nil || name != "beta" || id != "w1:p1" {
		t.Fatalf("Resolve(beta/w1:p1) = (%q,%v,%q,%v)", name, c != nil, id, err)
	}
	if c.Socket() != "/nonexistent/beta.sock" {
		t.Fatalf("routed to the wrong socket: %s", c.Socket())
	}
	if _, _, _, err := s.Resolve("ghost/w1:p1"); err == nil {
		t.Fatal("an unknown session must not resolve")
	}
}
