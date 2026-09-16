package core

import "sync"

// SeenSet is a PER-CONNECTION unseen tracker.
//
// Herdr's "done" is idle-but-unseen. With a TUI, a browser and two phones all
// attached, a global seen set means any one of them wipes everyone else's DONE
// badges. So the store holds only the authoritative idle transition counter per
// pane, and each connection derives DONE from its own seen marks.
//
// A connection marks a pane seen when it focuses it. Reads never mark seen.
type SeenSet struct {
	mu   sync.Mutex
	seen map[string]int64 // paneID -> the doneSeq this connection has acknowledged
}

// NewSeenSet builds an empty per-connection seen set.
func NewSeenSet() *SeenSet { return &SeenSet{seen: make(map[string]int64)} }

// MarkSeen acknowledges the pane's current doneSeq for this connection only.
func (s *SeenSet) MarkSeen(paneID string, doneSeq int64) {
	s.mu.Lock()
	if doneSeq > s.seen[paneID] {
		s.seen[paneID] = doneSeq
	}
	s.mu.Unlock()
}

// Unseen reports whether this connection still owes the user a DONE badge.
func (s *SeenSet) Unseen(paneID string, doneSeq int64) bool {
	if doneSeq == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[paneID] < doneSeq
}

// Forget drops a pane (it closed).
func (s *SeenSet) Forget(paneID string) {
	s.mu.Lock()
	delete(s.seen, paneID)
	s.mu.Unlock()
}

// Decorate returns the subset of panes this connection considers DONE.
func (s *SeenSet) Decorate(done map[string]int64) map[string]bool {
	out := make(map[string]bool, len(done))
	s.mu.Lock()
	defer s.mu.Unlock()
	for pane, seq := range done {
		if seq > 0 && s.seen[pane] < seq {
			out[pane] = true
		}
	}
	return out
}
