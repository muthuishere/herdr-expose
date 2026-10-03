// Package msg is agent-to-agent messaging on top of Herdr.
//
// Every send gets a request id. The daemon that owns the TARGET agent keeps
// one file pair per request in <state>/messages:
//
//	<id>.request.json   who, to whom, body, hops, status
//	<id>.response.json  the reply, written once, atomically
//
// A response file is the proof of a round trip; nothing here reads an agent's
// screen. The store keeps the newest Keep requests and never sweeps one that
// is still waiting for its reply, until it expires.
package msg

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status of a request.
const (
	StatusQueued    = "queued"    // target busy or not found yet; retried
	StatusDelivered = "delivered" // typed into the target; waiting for a reply
	StatusReplied   = "replied"   // response file exists
	StatusBlocked   = "blocked"   // target is on a permission/question dialog
	StatusFailed    = "failed"    // Herdr rejected it for good
	StatusExpired   = "expired"   // nobody answered within Expiry
)

// DefaultKeep is how many requests survive a sweep.
const DefaultKeep = 100

// DefaultExpiry is how long an unanswered request is protected from a sweep.
const DefaultExpiry = 24 * time.Hour

// ErrNotFound is returned for an unknown request id.
var ErrNotFound = errors.New("no such request")

// Request is <id>.request.json.
type Request struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Body     string    `json:"body"`
	Hops     int       `json:"hops"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Attempts int       `json:"attempts"`
	// DeliveredAt is set once the text was typed into the target. After that
	// the request is never typed again; only its status follows the target.
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// Response is <id>.response.json.
type Response struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
}

// Store is the file store. Safe for concurrent use within one process.
type Store struct {
	dir    string
	keep   int
	expiry time.Duration
	now    func() time.Time
	mu     sync.Mutex
	// delivering holds the requests whose text is being typed right now. It is
	// in memory only, which is enough: one daemon owns this directory, and the
	// CLI is a thin HTTP client of it rather than a second writer.
	delivering map[string]bool
}

// NewStore opens (creating) <dir>.
func NewStore(dir string, keep int, expiry time.Duration) (*Store, error) {
	if keep <= 0 {
		keep = DefaultKeep
	}
	if expiry <= 0 {
		expiry = DefaultExpiry
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, keep: keep, expiry: expiry, now: time.Now}, nil
}

// Claim reserves a request for delivery, reporting whether the caller got it.
// A delivery is slow — an agent listing, then a prompt that has to reach Herdr
// and be typed into a pane — and delivered_at is written only once the prompt
// returns. For that whole window the stored request still reads "queued", so
// without a claim the retry loop picks it up and types the same envelope a
// second time. Release it when the attempt is over, whatever its outcome.
func (s *Store) Claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivering[id] {
		return false
	}
	if s.delivering == nil {
		s.delivering = make(map[string]bool)
	}
	s.delivering[id] = true
	return true
}

// Release gives up a claim taken by Claim.
func (s *Store) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.delivering, id)
}

var idRe = regexp.MustCompile(`^r-[0-9]{13}-[0-9a-f]{4}$`)

// NewID is r-<unix-ms>-<4 hex>: unique enough, and sorts by time.
func NewID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("r-%013d-%s", now.UnixMilli(), hex.EncodeToString(b[:]))
}

// ValidID reports whether id can name a file in the store.
func ValidID(id string) bool { return idRe.MatchString(id) }

func (s *Store) reqPath(id string) string  { return filepath.Join(s.dir, id+".request.json") }
func (s *Store) respPath(id string) string { return filepath.Join(s.dir, id+".response.json") }

// writeAtomic writes via a temp file and rename, so a reader never sees half.
func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Create stores a new request and sweeps.
func (s *Store) Create(from, to, body string, hops int) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r := Request{ID: NewID(now), From: from, To: to, Body: body, Hops: hops,
		Created: now, Updated: now, Status: StatusQueued}
	if err := writeAtomic(s.reqPath(r.ID), r); err != nil {
		return Request{}, err
	}
	s.sweepLocked()
	return r, nil
}

// Get returns the request and its response, if any.
func (s *Store) Get(id string) (Request, *Response, error) {
	if !ValidID(id) {
		return Request{}, nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Request
	if err := readJSON(s.reqPath(id), &r); err != nil {
		return Request{}, nil, err
	}
	var resp Response
	switch err := readJSON(s.respPath(id), &resp); {
	case err == nil:
		return r, &resp, nil
	case errors.Is(err, ErrNotFound):
		return r, nil, nil
	default:
		return r, nil, err
	}
}

// Update applies fn to a stored request and writes it back.
func (s *Store) Update(id string, fn func(*Request)) (Request, error) {
	if !ValidID(id) {
		return Request{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Request
	if err := readJSON(s.reqPath(id), &r); err != nil {
		return Request{}, err
	}
	fn(&r)
	r.Updated = s.now()
	return r, writeAtomic(s.reqPath(id), r)
}

// Reply writes the response file once and marks the request replied.
func (s *Store) Reply(id, from, body string) (Response, error) {
	if !ValidID(id) {
		return Response{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Request
	if err := readJSON(s.reqPath(id), &r); err != nil {
		return Response{}, err
	}
	if _, err := os.Stat(s.respPath(id)); err == nil {
		return Response{}, fmt.Errorf("request %s already has a reply", id)
	}
	now := s.now()
	resp := Response{ID: id, From: from, Body: body, Created: now}
	if err := writeAtomic(s.respPath(id), resp); err != nil {
		return Response{}, err
	}
	r.Status, r.Error, r.Updated = StatusReplied, "", now
	if err := writeAtomic(s.reqPath(id), r); err != nil {
		return Response{}, err
	}
	s.sweepLocked()
	return resp, nil
}

// List returns requests newest first, optionally only those with a status.
func (s *Store) List(status string) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.idsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		var r Request
		if readJSON(s.reqPath(ids[i]), &r) != nil {
			continue
		}
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// idsLocked lists request ids oldest first (the id sorts by time).
func (s *Store) idsLocked() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".request.json"); ok && ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func pending(status string) bool {
	return status == StatusQueued || status == StatusDelivered || status == StatusBlocked
}

// Sweep expires stale pending requests, then deletes the oldest pairs until
// at most keep remain. A request still waiting for its reply is never deleted.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
}

func (s *Store) sweepLocked() {
	ids, err := s.idsLocked()
	if err != nil {
		return
	}
	now := s.now()
	type entry struct {
		id      string
		pending bool
	}
	entries := make([]entry, 0, len(ids))
	for _, id := range ids {
		var r Request
		if readJSON(s.reqPath(id), &r) != nil {
			entries = append(entries, entry{id: id})
			continue
		}
		if pending(r.Status) && now.Sub(r.Created) > s.expiry {
			r.Status, r.Updated = StatusExpired, now
			_ = writeAtomic(s.reqPath(id), r)
		}
		entries = append(entries, entry{id: id, pending: pending(r.Status)})
	}
	excess := len(entries) - s.keep
	for _, e := range entries {
		if excess <= 0 {
			break
		}
		if e.pending {
			continue
		}
		os.Remove(s.respPath(e.id))
		os.Remove(s.reqPath(e.id))
		excess--
	}
}
