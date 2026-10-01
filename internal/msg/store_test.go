package msg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T, keep int) (*Store, *time.Time) {
	t.Helper()
	s, err := NewStore(t.TempDir(), keep, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { now = now.Add(time.Millisecond); return now }
	return s, &now
}

func TestCreateGetReply(t *testing.T) {
	s, _ := newTestStore(t, 10)
	r, err := s.Create("a", "b", "hi", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(r.ID) {
		t.Fatalf("bad id %q", r.ID)
	}
	got, resp, err := s.Get(r.ID)
	if err != nil || resp != nil || got.Status != StatusQueued {
		t.Fatalf("get before reply: %+v %v %v", got, resp, err)
	}
	if _, err := s.Reply(r.ID, "b", "hello"); err != nil {
		t.Fatal(err)
	}
	got, resp, err = s.Get(r.ID)
	if err != nil || resp == nil || resp.Body != "hello" || got.Status != StatusReplied {
		t.Fatalf("get after reply: %+v %+v %v", got, resp, err)
	}
	if _, err := s.Reply(r.ID, "b", "again"); err == nil {
		t.Fatal("second reply must be refused")
	}
}

func TestUnknownAndInvalidIDs(t *testing.T) {
	s, _ := newTestStore(t, 10)
	for _, id := range []string{"r-0000000000001-abcd", "../../etc/passwd", ""} {
		if _, _, err := s.Get(id); err != ErrNotFound {
			t.Fatalf("Get(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := s.Reply(id, "x", "y"); err != ErrNotFound {
			t.Fatalf("Reply(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestSweepKeepsNewestAndPending(t *testing.T) {
	s, _ := newTestStore(t, 3)
	var ids []string
	for i := 0; i < 6; i++ {
		r, err := s.Create("a", "b", "x", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
		if i != 1 { // ids[1] stays pending
			if _, err := s.Reply(r.ID, "b", "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}
	all, _ := s.List("")
	if len(all) != 3 { // the pending one counts toward keep: it + 2 newest
		t.Fatalf("kept %d, want 3", len(all))
	}
	if all[0].ID != ids[5] || all[1].ID != ids[4] {
		t.Fatalf("newest not kept: %v", all)
	}
	if _, _, err := s.Get(ids[1]); err != nil {
		t.Fatal("pending request was swept")
	}
	if _, _, err := s.Get(ids[0]); err != ErrNotFound {
		t.Fatal("oldest answered request survived")
	}
	if _, err := os.Stat(filepath.Join(s.dir, ids[0]+".response.json")); !os.IsNotExist(err) {
		t.Fatal("response file of swept request survived")
	}
}

func TestSweepExpiresStalePending(t *testing.T) {
	s, now := newTestStore(t, 1)
	old, _ := s.Create("a", "b", "x", 0)
	*now = now.Add(2 * time.Hour)
	if _, err := s.Create("a", "b", "y", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(old.ID); err != ErrNotFound {
		t.Fatalf("expired request should be swept, got %v", err)
	}
}
