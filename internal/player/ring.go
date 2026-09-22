package player

import "sync/atomic"

// ring is a lock-free single-producer/single-consumer float32 buffer.
// The decoder goroutine writes; the audio callback reads. Neither ever blocks.
type ring struct {
	buf  []float32
	mask uint64
	w    atomic.Uint64 // written frames*channels
	r    atomic.Uint64
}

func newRing(n int) *ring {
	size := 1
	for size < n {
		size <<= 1 // power of two so index wrapping is a mask, not a modulo
	}
	return &ring{buf: make([]float32, size), mask: uint64(size - 1)}
}

func (q *ring) writable() int { return len(q.buf) - int(q.w.Load()-q.r.Load()) }
func (q *ring) readable() int { return int(q.w.Load() - q.r.Load()) }

// write copies as much of p as fits and returns how much it took.
func (q *ring) write(p []float32) int {
	n := q.writable()
	if n > len(p) {
		n = len(p)
	}
	w := q.w.Load()
	for i := 0; i < n; i++ {
		q.buf[(w+uint64(i))&q.mask] = p[i]
	}
	q.w.Store(w + uint64(n))
	return n
}

// read fills p and returns how much was available. Called from the audio callback:
// no allocation, no lock, no blocking.
func (q *ring) read(p []float32) int {
	n := q.readable()
	if n > len(p) {
		n = len(p)
	}
	r := q.r.Load()
	for i := 0; i < n; i++ {
		p[i] = q.buf[(r+uint64(i))&q.mask]
	}
	q.r.Store(r + uint64(n))
	return n
}

func (q *ring) reset() {
	q.r.Store(q.w.Load())
}
