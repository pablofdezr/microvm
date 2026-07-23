package logstore

// ringBuffer keeps the last N bytes written to it.
//
// Keeping the tail rather than the head is deliberate. When a program fails,
// the interesting part -- the traceback, the last thing it printed before it
// died -- is at the end. A head-truncating buffer would faithfully preserve the
// startup banner and throw away the error.
type ringBuffer struct {
	buf []byte
	// start is where the oldest byte lives once the buffer has wrapped.
	start int
	full  bool
	// truncated records that bytes were dropped, so callers are never handed a
	// partial log that looks whole.
	truncated bool
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, 0, size)}
}

// Write appends p, dropping the oldest bytes if it no longer fits.
func (r *ringBuffer) Write(p []byte) {
	capacity := cap(r.buf)
	if capacity == 0 {
		return
	}

	// A single write at least as big as the whole buffer: keep only its tail,
	// since everything before it is already unreachable.
	if len(p) >= capacity {
		// Truncation means bytes were actually lost, not that this branch was
		// taken. A write of exactly capacity into an empty buffer loses
		// nothing, and flagging it would tell callers their complete log is
		// partial.
		lostFromWrite := len(p) > capacity
		lostFromBuffer := len(r.buf) > 0
		r.truncated = r.truncated || lostFromWrite || lostFromBuffer

		r.buf = r.buf[:capacity]
		copy(r.buf, p[len(p)-capacity:])
		r.start = 0
		r.full = true
		return
	}

	// Still growing: no wrap needed yet.
	if !r.full {
		free := capacity - len(r.buf)
		if len(p) <= free {
			r.buf = append(r.buf, p...)
			if len(r.buf) == capacity {
				r.full = true
			}
			return
		}
		// This write is what fills it. Take what fits, then wrap the rest.
		r.buf = append(r.buf, p[:free]...)
		r.full = true
		r.start = 0
		p = p[free:]
	}

	// Wrapped: overwrite from start, advancing it past what we clobber.
	for _, b := range p {
		r.buf[r.start] = b
		r.start = (r.start + 1) % capacity
	}
	r.truncated = true
}

// Bytes returns the contents in write order, and whether anything was dropped.
func (r *ringBuffer) Bytes() ([]byte, bool) {
	if !r.full {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out, r.truncated
	}

	// Unwrap: oldest bytes run from start to the end, then wrap to start.
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.start:]...)
	out = append(out, r.buf[:r.start]...)
	return out, r.truncated
}

// Len is how many bytes are currently retained.
func (r *ringBuffer) Len() int { return len(r.buf) }
