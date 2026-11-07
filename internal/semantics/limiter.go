// Rate decision rules: one limiter per algorithm, for one key, in Unix milliseconds.

package semantics

// Limiter admits or refuses one request at a time; times never decrease.
type Limiter interface {
	Admit(now int64) bool
}

// Limiter returns a new limiter for one key, its window in milliseconds.
func (r Rate) Limiter() Limiter {
	window := r.Window * 1000
	switch r.Algorithm {
	case FixedAligned:
		return &fixed{count: r.Count, window: window, aligned: true}
	case FixedFirstRequest:
		return &fixed{count: r.Count, window: window, start: -window}
	case WeightedSliding:
		return &weighted{count: r.Count, window: window}
	case TokenBucket:
		return newBucket(r.Count, window, r.Burst)
	case LeakyBucket:
		return newBucket(r.Count, window, r.Burst+1)
	}
	return &slidingLog{count: r.Count, window: window}
}

// fixed counts per window: clock-aligned, or starting at the first request
// after the previous window ended.
type fixed struct {
	count, window, start, used int64
	aligned                    bool
}

func (f *fixed) Admit(now int64) bool {
	start := f.start
	if f.aligned {
		start = now / f.window * f.window
	} else if now >= f.start+f.window {
		start = now
	}
	if start != f.start {
		f.start, f.used = start, 0
	}
	if f.used >= f.count {
		return false
	}
	f.used++
	return true
}

// weighted is the gateway's rule: with C admitted in the current
// epoch-aligned bucket, P in the previous one and d into the current one,
// admit when C + P*(W-d)/W + 1 <= N, in exact integer arithmetic.
type weighted struct {
	count, window, bucket, current, previous int64
}

func (w *weighted) Admit(now int64) bool {
	bucket := now / w.window
	switch {
	case bucket == w.bucket+1:
		w.previous, w.current = w.current, 0
	case bucket > w.bucket+1:
		w.previous, w.current = 0, 0
	}
	w.bucket = bucket
	elapsed := now - bucket*w.window
	if w.current*w.window+w.previous*(w.window-elapsed)+w.window > w.count*w.window {
		return false
	}
	w.current++
	return true
}

// bucket holds tokens scaled by the window, refilled count per window and
// capped at capacity; it starts full.
type bucket struct {
	count, window, capacity, tokens, last int64
}

func newBucket(count, window, capacity int64) *bucket {
	return &bucket{count: count, window: window, capacity: capacity * window, tokens: capacity * window}
}

func (b *bucket) Admit(now int64) bool {
	if now > b.last {
		b.tokens = min(b.capacity, b.tokens+(now-b.last)*b.count)
		b.last = now
	}
	if b.tokens < b.window {
		return false
	}
	b.tokens -= b.window
	return true
}

// slidingLog admits while fewer than count were admitted in the last window.
type slidingLog struct {
	count, window int64
	admitted      []int64
}

func (s *slidingLog) Admit(now int64) bool {
	for len(s.admitted) > 0 && s.admitted[0] <= now-s.window {
		s.admitted = s.admitted[1:]
	}
	if int64(len(s.admitted)) >= s.count {
		return false
	}
	s.admitted = append(s.admitted, now)
	return true
}
