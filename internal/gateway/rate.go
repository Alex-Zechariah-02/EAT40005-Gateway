// Weighted sliding-window rate limiter and its counters.

package gateway

import (
	"container/heap"
	"net/http"
	"sync"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

// counterKey identifies one quota: an operation and the trusted key it is
// counted by. It does not include the generation, so publishing a policy
// never grants a fresh quota.
type counterKey struct {
	operation string
	keyType   string
	key       string
}

// counter is the state of one approximate sliding window: the requests
// admitted in the current bucket and in the bucket before it. Buckets are
// window milliseconds long and anchored at the Unix epoch.
type counter struct {
	window   int64
	bucket   int64
	current  int64
	previous int64
	key      counterKey
	index    int // position in the limiter's expiry queue
}

// removable is the first time at which both of the counter's buckets have
// ended, so its estimate is zero: now/window >= bucket+2.
func (c *counter) removable() int64 {
	return (c.bucket + 2) * c.window
}

// expiryQueue orders counters by the time they become removable, so a sweep
// visits only counters it removes. It implements heap.Interface.
type expiryQueue []*counter

func (q expiryQueue) Len() int           { return len(q) }
func (q expiryQueue) Less(i, j int) bool { return q[i].removable() < q[j].removable() }

func (q expiryQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}

func (q *expiryQueue) Push(x any) {
	c := x.(*counter)
	c.index = len(*q)
	*q = append(*q, c)
}

func (q *expiryQueue) Pop() any {
	old := *q
	c := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	return c
}

// roll moves the counter to the bucket containing now. A time before the
// current bucket leaves it unchanged.
func (c *counter) roll(now int64) {
	bucket := now / c.window
	switch {
	case bucket == c.bucket+1:
		c.previous, c.current = c.current, 0
	case bucket > c.bucket+1:
		c.previous, c.current = 0, 0
	}
	if bucket > c.bucket {
		c.bucket = bucket
	}
}

// usage returns the weighted request count multiplied by the window, so the
// admission rule is exact integer arithmetic. A time before the bucket start
// weighs the previous bucket fully.
func (c *counter) usage(now int64) int64 {
	elapsed := max(now-c.bucket*c.window, 0)
	return c.current*c.window + c.previous*(c.window-elapsed)
}

type rateDecision int

const (
	rateAdmitted rateDecision = iota
	rateExceeded
	rateNoCapacity
)

// limiter holds the counters of every rate rule on this gateway node. One
// mutex makes each check and increment a single atomic decision.
type limiter struct {
	mu         sync.Mutex
	counters   map[counterKey]*counter
	expiry     expiryQueue // the same counters, earliest removable first
	admissions int
	capacity   int
}

func newLimiter() *limiter {
	return &limiter{counters: map[counterKey]*counter{}, capacity: compile.MaxCounters}
}

// admit applies the rule at now (Unix milliseconds): with C requests admitted
// in the current bucket, P in the previous one and offset d into the current
// bucket, the request is admitted when C + P*(W-d)/W + 1 <= N, and only an
// admitted request is counted.
func (l *limiter) admit(key counterKey, rule *compile.RateRule, now int64) rateDecision {
	l.mu.Lock()
	defer l.mu.Unlock()

	c := l.counters[key]
	if c == nil {
		if len(l.counters) >= l.capacity {
			l.sweep(now)
		}
		if len(l.counters) >= l.capacity {
			return rateNoCapacity
		}
		c = &counter{window: rule.Window(), bucket: now / rule.Window(), key: key}
		l.counters[key] = c
		heap.Push(&l.expiry, c)
	}

	removable := c.removable()
	c.roll(now)
	if c.window != rule.Window() {
		// The window changed: carry the usage so far, rounded up, into a
		// counter for the new window, so a change never forgives requests.
		used := (c.usage(now) + c.window - 1) / c.window
		c.window, c.bucket, c.current, c.previous = rule.Window(), now/rule.Window(), used, 0
	}
	if c.removable() != removable {
		heap.Fix(&l.expiry, c.index)
	}

	if c.usage(now)+rule.Window() > rule.Count()*rule.Window() {
		return rateExceeded
	}
	c.current++

	l.admissions++
	if l.admissions%compile.SweepInterval == 0 {
		l.sweep(now)
	}
	return rateAdmitted
}

// sweep removes counters whose current and previous buckets have both ended.
// Their estimate is zero, so removing them never changes a decision. It visits
// only the counters it removes, so a full table of live counters costs no scan.
func (l *limiter) sweep(now int64) {
	for len(l.expiry) > 0 && l.expiry[0].removable() <= now {
		c := heap.Pop(&l.expiry).(*counter)
		delete(l.counters, c.key)
	}
}

// rateClock reads limiter time: a wall-clock anchor taken once plus the
// monotonic time elapsed since, so wall-clock steps do not move it.
type rateClock struct{ anchor time.Time }

func (c rateClock) now() int64 {
	if ms, ok := fault.RateClockFault(); ok {
		return ms
	}
	return c.anchor.UnixMilli() + time.Since(c.anchor).Milliseconds()
}

// limit runs the rate stage. The counter is the rule's (a conditioned
// operation shares its unconditioned sibling's), keyed by the verified
// subject or the direct socket peer; client forwarding headers never choose
// an address counter. It answers a refusal itself and reports whether the
// request may continue.
func (h *Handler) limit(q *request, rule *compile.Rule) bool {
	if rule.Rate() == nil {
		q.event.Stages[audit.StageRate] = audit.StageNotApplicable
		return true
	}
	key := counterKey{operation: rule.Counter(), keyType: rule.RateKey(), key: q.event.Subject}
	if rule.RateKey() == compile.RateKeyAddress {
		key.key = q.event.ClientAddress
	}
	if !h.admitRate(q, audit.StageRate, key, rule.Rate()) {
		return false
	}
	q.pass(audit.StageRate)
	return true
}

// admitRate counts one request on key and answers a refusal at stage when
// the limit is reached, the table is full or an address key is missing
// (every key type but the subject counts by client address).
func (h *Handler) admitRate(q *request, stage int, key counterKey, rule *compile.RateRule) bool {
	if key.keyType != compile.RateKeySubject && key.key == "" {
		q.event.Reason = "invalid_peer_address"
		q.reject(stage, http.StatusBadRequest, "bad_request")
		return false
	}
	switch h.controller.limiter.admit(key, rule, h.controller.clock.now()) {
	case rateExceeded:
		q.event.Reason = "quota_exceeded"
		q.reject(stage, http.StatusTooManyRequests, "rate_limited")
		return false
	case rateNoCapacity:
		q.event.Reason = "rate_capacity"
		q.reject(stage, http.StatusServiceUnavailable, "gateway_unavailable")
		return false
	}
	return true
}
