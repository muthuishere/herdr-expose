package core

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Metrics is a tiny latency instrument for the two budgeted paths.
//
// A claimed number is not a number: these are recorded on the real hot paths in
// the running server and exposed at /v1/metrics, so the budget in SPEC A3 can be
// checked against a live session rather than asserted.
type Metrics struct {
	Input  *Hist // websocket read -> upstream write returned (budget < 5ms)
	Output *Hist // upstream frame decoded -> queued for the socket
	Write  *Hist // queued -> websocket write returned

	mu     sync.Mutex
	frames uint64
	bytes  uint64
}

// NewMetrics builds an instrument.
func NewMetrics() *Metrics {
	return &Metrics{Input: NewHist(4096), Output: NewHist(4096), Write: NewHist(4096)}
}

// CountFrame records one outbound data-plane frame.
func (m *Metrics) CountFrame(n int) {
	m.mu.Lock()
	m.frames++
	m.bytes += uint64(n)
	m.mu.Unlock()
}

// Snapshot renders the current numbers.
func (m *Metrics) Snapshot() map[string]any {
	m.mu.Lock()
	frames, bytes := m.frames, m.bytes
	m.mu.Unlock()
	return map[string]any{
		"frames_out":       frames,
		"bytes_out":        bytes,
		"input_us":         m.Input.Snapshot(),
		"output_us":        m.Output.Snapshot(),
		"budget_input_us":  5000,
		"budget_output_us": 20000,
	}
}

// Hist is a fixed-capacity reservoir of microsecond samples.
// Fixed capacity means it allocates once, at construction, and never again.
type Hist struct {
	mu      sync.Mutex
	samples []float64
	n       int
	total   uint64
	max     float64
}

// NewHist builds a histogram with room for cap samples.
func NewHist(capacity int) *Hist {
	return &Hist{samples: make([]float64, capacity)}
}

// Observe records a duration.
func (h *Hist) Observe(d time.Duration) {
	us := float64(d.Microseconds())
	h.mu.Lock()
	h.samples[h.n%len(h.samples)] = us
	h.n++
	h.total++
	if us > h.max {
		h.max = us
	}
	h.mu.Unlock()
}

// Since is the common call shape.
func (h *Hist) Since(t time.Time) { h.Observe(time.Since(t)) }

// Snapshot returns count/p50/p95/p99/max in microseconds.
func (h *Hist) Snapshot() map[string]any {
	h.mu.Lock()
	n := h.n
	if n > len(h.samples) {
		n = len(h.samples)
	}
	buf := make([]float64, n)
	copy(buf, h.samples[:n])
	total, max := h.total, h.max
	h.mu.Unlock()

	if n == 0 {
		return map[string]any{"count": total}
	}
	sort.Float64s(buf)
	q := func(p float64) float64 {
		i := int(math.Ceil(p*float64(n))) - 1
		if i < 0 {
			i = 0
		}
		return buf[i]
	}
	return map[string]any{
		"count": total, "p50": q(0.50), "p95": q(0.95), "p99": q(0.99), "max": max,
	}
}
