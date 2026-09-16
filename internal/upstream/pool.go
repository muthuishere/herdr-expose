package upstream

import (
	"sync"
	"sync/atomic"
)

// Buf is a refcounted, pooled byte buffer.
//
// It exists so a terminal frame is encoded ONCE in the hub and the exact same
// bytes are handed to every subscribed client and to the replay ring, with no
// per-client copy and no steady-state allocation. The buffer returns to its
// size-class pool when the last holder releases it.
//
// Ownership rule: whoever hands a Buf to someone else calls Retain first; every
// holder calls Release exactly once. Never mutate B after the first Retain.
type Buf struct {
	B    []byte
	refs atomic.Int32
	cls  int
}

// Len is the current payload length.
func (b *Buf) Len() int { return len(b.B) }

// Retain adds one reference.
func (b *Buf) Retain() *Buf {
	b.refs.Add(1)
	return b
}

// RetainN adds n references at once (fanout to n clients).
func (b *Buf) RetainN(n int32) *Buf {
	if n > 0 {
		b.refs.Add(n)
	}
	return b
}

// Release drops one reference, recycling the buffer at zero.
func (b *Buf) Release() {
	if b == nil {
		return
	}
	if n := b.refs.Add(-1); n > 0 {
		return
	} else if n < 0 {
		panic("upstream: Buf released more times than retained")
	}
	b.B = b.B[:0]
	pools[b.cls].Put(b)
}

// size classes: 1KiB .. 8MiB, powers of two.
const (
	minClassShift = 10
	numClasses    = 14
)

var pools [numClasses]*sync.Pool

func init() {
	for i := range pools {
		cls := i
		pools[i] = &sync.Pool{New: func() any {
			return &Buf{B: make([]byte, 0, 1<<(minClassShift+cls)), cls: cls}
		}}
	}
}

func classFor(n int) int {
	c := 0
	for sz := 1 << minClassShift; sz < n; sz <<= 1 {
		c++
		if c >= numClasses-1 {
			return numClasses - 1
		}
	}
	return c
}

// GetBuf returns a buffer with len 0 and capacity >= n, refcount 1.
func GetBuf(n int) *Buf {
	c := classFor(n)
	b := pools[c].Get().(*Buf)
	if cap(b.B) < n {
		// only for the oversized top class
		b.B = make([]byte, 0, n)
	}
	b.B = b.B[:0]
	b.refs.Store(1)
	return b
}

// BufOf copies p into a fresh pooled buffer with refcount 1.
func BufOf(p []byte) *Buf {
	b := GetBuf(len(p))
	b.B = append(b.B, p...)
	return b
}
